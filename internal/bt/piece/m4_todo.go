package piece

// M4 placeholders so the tree builds; the implementer replaces this file and
// reworks picker.go to the M4 contract (ADR-0018).

// Interesting is not implemented yet (M4).
func (pk *picker) Interesting(PeerKey) bool { panic("piece: Interesting not implemented (M4)") }

// IsSeed is not implemented yet (M4).
func (pk *picker) IsSeed(PeerKey) bool { panic("piece: IsSeed not implemented (M4)") }

// Expire is not implemented yet (M4).
func (pk *picker) Expire() []Expired { return nil }

// SetPriority is not implemented yet (M4).
func (pk *picker) SetPriority(int, Priority) { panic("piece: SetPriority not implemented (M4)") }

// Endgame is not implemented yet (M4).
func (pk *picker) Endgame() bool { return false }
