package sessions

// InFlightDeliveries snapshots the deliveries that shutdown must wait for.
// sendMu pairs with claim registration, after the shared stopping flag is set.
func (sess *Delivery) InFlightDeliveries() []chan struct{} {
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	var inFlight []chan struct{}
	for _, op := range sess.sendOps {
		select {
		case <-op.done:
		default:
			inFlight = append(inFlight, op.done)
		}
	}
	return inFlight
}
