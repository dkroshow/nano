package statewiring

import (
	"context"
	"strings"
	"testing"

	statedb "github.com/dkroshow/nano/internal/state/db"
)

func TestMigrationsRejectUnknownStage(t *testing.T) {
	err := Migrations()(context.Background(), statedb.MigrationStage(255), nil, nil, 0, "")
	if err == nil || !strings.Contains(err.Error(), "unknown migration stage") {
		t.Fatalf("unknown stage error = %v", err)
	}
}
