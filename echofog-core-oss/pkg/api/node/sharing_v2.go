package node

// SharingApprove confirms a client waiting for the owner and remembers it under label.
func (n *NodeApi) SharingApprove(id, label string) error { return ErrV2NotBuilt }

// SharingReject turns a waiting client down.
func (n *NodeApi) SharingReject(id string) error { return ErrV2NotBuilt }

// SharingRevoke forgets a confirmed client and closes its streams.
func (n *NodeApi) SharingRevoke(id string) error { return ErrV2NotBuilt }

// SharingOpenFor lets anyone with the link use the exit for the given minutes.
func (n *NodeApi) SharingOpenFor(minutes int) error { return ErrV2NotBuilt }

// SharingCloseOpen ends the open access.
func (n *NodeApi) SharingCloseOpen() error { return ErrV2NotBuilt }
