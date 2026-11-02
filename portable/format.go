package portable

import "fmt"

type StringFormats struct {
	Date                string `json:"date"`
	Time                string `json:"time"`
	DateTime            string `json:"dateTime"`
	UUID                string `json:"uuid"`
	JSONPointer         string `json:"jsonPointer"`
	RelativeJSONPointer string `json:"relativeJSONPointer"`
}

// Only formats with an equivalent SDK predicate are admitted. Unknown format
// names must not silently become unconstrained values on another runtime.
func ProfileStringFormats() StringFormats {
	return StringFormats{
		Date: "date", Time: "time", DateTime: "date-time", UUID: "uuid",
		JSONPointer: "json-pointer", RelativeJSONPointer: "relative-json-pointer",
	}
}

func checkFormat(value any) error {
	formats := ProfileStringFormats()
	switch value {
	case formats.Date, formats.Time, formats.DateTime, formats.UUID, formats.JSONPointer, formats.RelativeJSONPointer:
		return nil
	default:
		return fmt.Errorf("portable: unsupported schema format %q", value)
	}
}
