package sessions

// InFlightDeliveries snapshots the deliveries that shutdown must wait for.
// sendMu pairs with claim registration, after the shared stopping flag is set.
func (delivery *Delivery) InFlightDeliveries() []chan struct{} {
	delivery.sendMu.Lock()
	defer delivery.sendMu.Unlock()
	var inFlight []chan struct{}
	for _, op := range delivery.sendOps {
		select {
		case <-op.done:
		default:
			inFlight = append(inFlight, op.done)
		}
	}
	return inFlight
}
