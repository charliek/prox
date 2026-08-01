package tui

// hitRegistry holds all render-recorded mouse hit geometry. It is
// heap-allocated and SHARED across ClientModel copies: ClientModel.View is
// a value receiver, so render-time writes to plain fields would be discarded
// (plan 022 WS0 — menu/chip clicks were dead in the live app). Safe because
// bubbletea's render loop is single-threaded: each frame's View fully
// rewrites the registry before the next Update reads it.
type hitRegistry struct {
	menuCells   []menuCellHit
	dropdown    menuDropdownHit // written IN PLACE, never re-pointed
	hasDropdown bool
	chips       []processChipHit
}

// menuCellHit records one top-level cell's clickable rect for this frame.
type menuCellHit struct {
	ID   MenuID
	Rect HitRect
}

// menuDropdownHit is the last-drawn dropdown hit-map (strix stale-rect discipline).
type menuDropdownHit struct {
	Menu   MenuID
	Bounds HitRect
	Rows   []menuRowHit
}

type menuRowHit struct {
	Cmd  MenuCommand // empty = separator / non-activatable
	Rect HitRect
}

// processChipHit records one process-panel chip's clickable rect for this frame.
type processChipHit struct {
	Index int
	Rect  HitRect
}

// ensureHits returns the shared hit registry, allocating lazily so tests that
// construct &BaseModel{} without newBaseModel never nil-pointer on access.
func (b *BaseModel) ensureHits() *hitRegistry {
	if b.hits == nil {
		b.hits = &hitRegistry{}
	}
	return b.hits
}
