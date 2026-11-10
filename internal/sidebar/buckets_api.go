package sidebar

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/threadindex"
)

// Every bucket route answers with the whole BucketState, so the browser that
// made the change and the browsers that hear of it apply the same value.

// HandleBuckets reads the current bucket state.
func (s *Controller) HandleBuckets(w http.ResponseWriter, r *http.Request) {
	s.writeBucketResult(w, http.StatusOK, func(store *Stars) (BucketState, error) { return store.buckets() })
}

// HandleBucketCreate appends a named bucket after the existing ones.
func (s *Controller) HandleBucketCreate(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Name string `json:"name"`
	}
	if !decodeBucketBody(w, r, &payload) {
		return
	}
	s.writeBucketResult(w, http.StatusCreated, func(store *Stars) (BucketState, error) { return store.createBucket(payload.Name) })
}

// HandleBucketRename renames one bucket.
func (s *Controller) HandleBucketRename(w http.ResponseWriter, r *http.Request, id string) {
	var payload struct {
		Name string `json:"name"`
	}
	if !decodeBucketBody(w, r, &payload) {
		return
	}
	s.writeBucketResult(w, http.StatusOK, func(store *Stars) (BucketState, error) { return store.renameBucket(id, payload.Name) })
}

// HandleBucketDelete removes one bucket; its threads become ungrouped.
func (s *Controller) HandleBucketDelete(w http.ResponseWriter, r *http.Request, id string) {
	s.writeBucketResult(w, http.StatusOK, func(store *Stars) (BucketState, error) { return store.deleteBucket(id) })
}

// HandleBucketOrder sets the operator's bucket order.
func (s *Controller) HandleBucketOrder(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		BucketIDs []string `json:"bucketIds"`
	}
	if !decodeBucketBody(w, r, &payload) {
		return
	}
	s.writeBucketResult(w, http.StatusOK, func(store *Stars) (BucketState, error) { return store.orderBuckets(payload.BucketIDs) })
}

// HandleThreadBucket puts a thread in a bucket, or takes it out with null.
func (s *Controller) HandleThreadBucket(w http.ResponseWriter, r *http.Request, rawID string) {
	if s.threadStars == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "buckets_unavailable", "Sidebar buckets store unavailable")
		return
	}
	thread, ok := s.resolveCatalogThread(w, r, rawID)
	if !ok {
		return
	}
	// bucketId is required: an explicit null takes the thread out, and a
	// body that forgot the field must not do the same silently.
	var payload struct {
		BucketID json.RawMessage `json:"bucketId"`
	}
	var bucketID string
	if !decodeBucketBody(w, r, &payload) {
		return
	}
	if len(payload.BucketID) == 0 || (string(payload.BucketID) != "null" && json.Unmarshal(payload.BucketID, &bucketID) != nil) {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_bucket_request", `Request body must be {"bucketId": "<id>"} or {"bucketId": null}`)
		return
	}
	s.writeBucketResult(w, http.StatusOK, func(store *Stars) (BucketState, error) {
		return store.assignBucket(thread.Provider, thread.NativeID, bucketID)
	})
}

func decodeBucketBody(w http.ResponseWriter, r *http.Request, payload any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, threadindex.MaxThreadNameRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(payload); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_bucket_request", "Request body is not valid bucket JSON")
		return false
	}
	return true
}

func (s *Controller) writeBucketResult(w http.ResponseWriter, status int, run func(*Stars) (BucketState, error)) {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.threadStars == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "buckets_unavailable", "Sidebar buckets store unavailable")
		return
	}
	state, err := run(s.threadStars)
	switch {
	case err == nil:
		threadindex.WriteCatalogJSON(w, status, state)
	case errors.Is(err, errBucketNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "bucket_not_found", "Bucket not found")
	case errors.Is(err, errBucketName):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_bucket_name", err.Error())
	case errors.Is(err, errBucketOrderStale):
		httpapi.WriteError(w, http.StatusConflict, "bucket_order_stale", err.Error())
	case errors.Is(err, errStarIdentityMissing):
		httpapi.WriteError(w, http.StatusConflict, "bucket_identity_missing", "Thread has no provider-native id to put in a bucket yet")
	default:
		httpapi.WriteError(w, http.StatusInternalServerError, "bucket_error", "Sidebar bucket update failed")
	}
}

// subscribeBuckets returns a stream's wake channel and its release; nil when
// the store is absent, which never fires.
func (s *Controller) subscribeBuckets() (<-chan struct{}, func()) {
	if s == nil || s.threadStars == nil {
		return nil, func() {}
	}
	ch := s.threadStars.bucketEvents.subscribe()
	return ch, func() { s.threadStars.bucketEvents.unsubscribe(ch) }
}

// writeBucketsEvent sends the whole current state as one `buckets` event.
func (s *Controller) writeBucketsEvent(w io.Writer) {
	state, err := s.threadStars.buckets()
	if err != nil {
		log.Printf("sidebar buckets read for event stream failed: %v", err)
		return
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: buckets\ndata: %s\n\n", payload)
}
