package portable

import "regexp"

var exportName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

func ValidExportName(name string) bool { return exportName.MatchString(name) }
