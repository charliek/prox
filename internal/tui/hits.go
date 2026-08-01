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
	Cmd   MenuCommand // empty = separator / indicator / non-activatable
	Index int         // full-list items index; -1 when non-activatable
	Rect  HitRect
}

// processChipHit records one process-panel chip's clickable rect for this frame.
type processChipHit struct {
	Index int
	Rect  HitRect
}

// resetFrame clears every hit slice and zeroes the dropdown object so stale
// rects cannot survive a frame where their renderer did not re-record
// (plan 023 A1 / B1). Called at the top of ClientModel.View.
func (h *hitRegistry) resetFrame() {
	h.menuCells = h.menuCells[:0]
	h.chips = h.chips[:0]
	h.dropdown = menuDropdownHit{}
	h.hasDropdown = false
}

// mustHits returns the shared hit registry. Panics if nil — there is no
// lazy-alloc path (that silently reintroduced the View value-copy bug).
// Production models get a registry from newBaseModel; tests use newTestBaseModel.
func (b *BaseModel) mustHits() *hitRegistry {
	if b.hits == nil {
		panic("tui: hitRegistry is nil; construct via newBaseModel or newTestBaseModel")
	}
	return b.hits
}
