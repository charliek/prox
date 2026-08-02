package tui

// Selection-band helpers (plan 023 E1 / C16).
//
// FullFill cursor rows rebuild segments with SelectionBG (via sel styleSet) and
// pad with s.Selection so every cell of every wrapped display row is on the
// band. SearchHighlight keeps SearchHitBG (search-hit precedence). Legacy
// (FullFill=false) never enters the band path — marker-only cursor rendering
// stays byte-identical to pre-C16 (C5 pins).

// inSelectionBand reports whether viewport content row contentRow is part of
// the active cursor selection band (logs: First..Last wrap span; requests:
// the single cursor row). Detail view has no cursor.
func (b *BaseModel) inSelectionBand(contentRow int) bool {
	th := CurrentTheme()
	if th == nil || !th.FullFill {
		return false
	}
	switch b.viewMode {
	case ViewModeRequests:
		return b.cursorIdx >= 0 && contentRow == b.cursorIdx
	case ViewModeLogs:
		if b.logSearchQuery == "" || b.logCursorIdx < 0 {
			return false
		}
		sp, ok := b.logRowSpans[b.logCursorSeq]
		if !ok {
			return false
		}
		return contentRow >= sp.First && contentRow <= sp.Last
	default:
		return false
	}
}
