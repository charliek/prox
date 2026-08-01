package tui

// newTestBaseModel returns a bare BaseModel with a live hitRegistry.
// Prefer newTestModel() for full ClientModel tests; use this when a bare
// BaseModel is intentional. There is no lazy-alloc path — mustHits panics
// on nil (plan 023 A1).
func newTestBaseModel() *BaseModel {
	return &BaseModel{hits: &hitRegistry{}}
}
