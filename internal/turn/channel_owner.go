package turn

// forChannel is the view a request from channel runs as. The owner is always
// resolved from channel alone, whatever owner the receiver view carries.
func (c *Coordinator) forChannel(channel string) (*Coordinator, error) {
	owner, err := c.owners.Owner(channel)
	if err != nil {
		return nil, err
	}
	if owner == c.ownerID && channel == c.channel {
		return c, nil
	}
	return &Coordinator{coordinatorState: c.coordinatorState, text: c.text, ownerID: owner, channel: channel}, nil
}
