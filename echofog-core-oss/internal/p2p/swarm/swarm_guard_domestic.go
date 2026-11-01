package swarm

// SetDomesticFunc tells the swarm which servers are in the node's own country, so a guard is
// picked there first when the node sits behind the TSPU freeze: a link that stays inside the
// country never crosses the border where a TCP connection to a foreign address freezes after
// 16 KB, and the guard relays what has to cross. nil (the default) ranks no server above another.
// The node layer supplies it only where that matters.
func (s *Swarm) SetDomesticFunc(f func(addr string) bool) {
	if f == nil {
		s.domestic.Store(nil)
		return
	}
	s.domestic.Store(&f)
}

// isDomestic reports whether the server at addr is in the node's own country, as the node layer
// says; false when it said nothing.
func (s *Swarm) isDomestic(addr string) bool {
	f := s.domestic.Load()
	return f != nil && (*f)(addr)
}
