package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type storedObservationIdentity struct {
	Scope      int64  `json:"s"`
	Source     int64  `json:"r"`
	Path       int64  `json:"p"`
	Line       int64  `json:"l"`
	Frame      int64  `json:"f"`
	Start      int64  `json:"o"`
	Length     int64  `json:"n"`
	SourceHash []byte `json:"h"`
	Facts      []byte `json:"v"`
}

func (stored storedObservationIdentity) digest() ([sha256.Size]byte, error) {
	raw, err := json.Marshal(stored)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

func writeTranscriptObservationTx(ctx context.Context, tx *sql.Tx, projectionID int64, observation model.Observation) error {
	if observation.ID == "" {
		return fmt.Errorf("transcript observation requires an identity")
	}
	key, err := internTranscriptValueTx(ctx, tx, projectionID, observation.ID)
	if err != nil {
		return err
	}
	scope, err := internTranscriptValueTx(ctx, tx, projectionID, observation.Scope)
	if err != nil {
		return err
	}
	base := observation.Source
	base.Line, base.Frame, base.Start, base.End = 0, 0, 0, 0
	base.BlockPath, base.SHA256 = "", ""
	source, err := internTranscriptValueTx(ctx, tx, projectionID, base)
	if err != nil {
		return err
	}
	path, err := internTranscriptValueTx(ctx, tx, projectionID, observation.Source.BlockPath)
	if err != nil {
		return err
	}
	facts := storedObservationFacts{Kind: observation.Kind, NativeHash: observation.NativeEventDigest, Complete: observation.Complete, Outcome: observation.Outcome, Capture: observation.Capture, Metadata: observation.Metadata}
	if observation.NativeEventID != "" {
		facts.NativeEvent, err = internTranscriptValueTx(ctx, tx, projectionID, observation.NativeEventID)
		if err != nil {
			return err
		}
	}
	times, err := storeTranscriptTimesTx(ctx, tx, projectionID, observation.Times)
	if err != nil {
		return err
	}
	facts.Times, err = compactObservationTimes(times, scope, key)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		return err
	}
	encoded, err = encodeTranscriptFacts(encoded)
	if err != nil {
		return err
	}
	sourceHash, err := hex.DecodeString(observation.Source.SHA256)
	if err != nil || len(sourceHash) != 0 && len(sourceHash) != sha256.Size {
		return fmt.Errorf("invalid observation source digest")
	}
	digestSourceHash := sourceHash
	if len(digestSourceHash) == 0 {
		digestSourceHash = nil
	}
	stored := storedObservationIdentity{
		Scope: scope, Source: source, Path: path,
		Line: observation.Source.Line, Frame: observation.Source.Frame,
		Start: observation.Source.Start, Length: observation.Source.End - observation.Source.Start,
		SourceHash: digestSourceHash, Facts: encoded,
	}
	digest, err := stored.digest()
	if err != nil {
		return fmt.Errorf("digest stored transcript observation: %w", err)
	}
	var prior storedObservationIdentity
	var priorHash []byte
	err = tx.QueryRowContext(ctx, `SELECT scope_id,source_id,block_path_id,source_line,source_frame,byte_start,byte_length,source_sha256,facts_blob,sha256
FROM conversation_observations WHERE projection_id=? AND key_id=?`, projectionID, key).Scan(
		&prior.Scope, &prior.Source, &prior.Path, &prior.Line, &prior.Frame,
		&prior.Start, &prior.Length, &prior.SourceHash, &prior.Facts, &priorHash)
	if err == nil {
		priorDigest, digestErr := prior.digest()
		if digestErr != nil {
			return digestErr
		}
		if !bytes.Equal(priorDigest[:], digest[:]) {
			return fmt.Errorf("observation changed under an existing source-local identity: %w", ErrTranscriptConflict)
		}
		if bytes.Equal(priorHash, digest[:]) {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE conversation_observations SET sha256=? WHERE projection_id=? AND key_id=?`, digest[:], projectionID, key)
		return err
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_observations(projection_id,key_id,scope_id,source_id,block_path_id,source_line,source_frame,byte_start,byte_length,source_sha256,facts_blob,sha256) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, projectionID, key, scope, source, path, observation.Source.Line, observation.Source.Frame, observation.Source.Start, observation.Source.End-observation.Source.Start, sourceHash, encoded, digest[:])
	return err
}

func writeTranscriptRelationTx(ctx context.Context, tx *sql.Tx, projectionID int64, relation model.Relation) error {
	raw, err := json.Marshal(relation)
	if err != nil {
		return fmt.Errorf("encode transcript relation: %w", err)
	}
	key := sha256.Sum256(raw)
	stored, err := compactTranscriptRelationTx(ctx, tx, projectionID, relation)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_relations(projection_id,relation_key,relation_kind,from_item_id,to_item_id,from_observation_value_id,to_observation_value_id,relation_json) VALUES (?,?,?,'','',?,?,?)`, projectionID, key[:], relation.Kind, nullablePositiveID(stored.FromObservation), nullablePositiveID(stored.ToObservation), encoded)
	if err != nil {
		return fmt.Errorf("store transcript relation: %w", err)
	}
	return nil
}

func linkTranscriptObservationsTx(ctx context.Context, tx *sql.Tx, projectionID int64, kind, key string, observations []model.Observation) error {
	var statement string
	switch kind {
	case "item":
		statement = `INSERT OR IGNORE INTO conversation_item_observations(projection_id, item_id, observation_id) VALUES (?, ?, ?)`
	case "turn":
		statement = `INSERT OR IGNORE INTO conversation_turn_observations(projection_id, turn_key, observation_id) VALUES (?, ?, ?)`
	default:
		return fmt.Errorf("invalid transcript observation owner kind")
	}
	for _, observation := range observations {
		value, err := internTranscriptValueTx(ctx, tx, projectionID, observation.ID)
		if err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM conversation_observations WHERE projection_id=? AND key_id=?`, projectionID, value).Scan(&id); err != nil {
			return fmt.Errorf("link missing transcript observation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, statement, projectionID, key, id); err != nil {
			return fmt.Errorf("link transcript observation to %s: %w", kind, err)
		}
	}
	return nil
}
