package testsupport

import "github.com/dkroshow/nano/internal/lifecycle"

// Fixtures describe provider evidence explicitly; production always starts with
// lifecycle.New and a durable attachment epoch.
func LifecycleSnapshot(status string, active bool) lifecycle.Snapshot {
	connection := lifecycle.Connection(status)
	switch status {
	case "":
		connection = lifecycle.Ready
	case "reconnect_failed":
		connection = lifecycle.Unreachable
	case "reconnect_parked":
		connection = lifecycle.Parked
	}
	work := lifecycle.Idle
	if connection == lifecycle.Ready && active {
		work = lifecycle.Working
	}
	return lifecycle.Snapshot{State: lifecycle.State{Connection: connection, Work: work}, Epoch: 1}
}
