package output

import (
	"encoding/json"
	"io"
)

type jsonRenderer struct{}

func (jsonRenderer) Format() string { return "json" }

func (jsonRenderer) Render(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
