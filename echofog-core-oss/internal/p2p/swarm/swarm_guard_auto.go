package swarm

// SetGuardsFree tells guard_mode "auto" whether the node is placed in a country that does not
// censor: there it stands down and the node dials the mesh directly (direct calls, hole punches),
// because no border on its side is watched. The node layer decides it from its own country; "on"
// and "off" ignore it.
func (s *Swarm) SetGuardsFree(free bool) {
	if s == nil {
		return
	}
	if s.guardsFree.Swap(free) != free {
		s.log().Info("Swarm: guard mode auto", "censoring_country", !free)
	}
}

// GuardsFree reports whether guard_mode "auto" stood down (see SetGuardsFree).
func (s *Swarm) GuardsFree() bool { return s != nil && s.guardsFree.Load() }
