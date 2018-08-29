package req

type ref struct {
	Exported    string `query:"exported"`
	notExported string
}

type ref2 struct {
	Exported    string `json:"exported"`
	notExported string
}

// POST /path
//
// Query: $ref: ref
// Request body: $ref: ref2
// Response 200: $empty
