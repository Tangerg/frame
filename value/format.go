package value

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

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
	if !slices.ContainsFunc(formatPredicates(), func(predicate formatPredicate) bool { return predicate.name == value }) {
		return fmt.Errorf("portable: unsupported schema format %q", value)
	}
	return nil
}

type formatPredicate struct {
	name   string
	accept func(string) bool
}

func (p formatPredicate) validate(value any) error {
	text, ok := value.(string)
	if ok && !p.accept(text) {
		return fmt.Errorf("portable: invalid %s", p.name)
	}
	return nil
}

func formatPredicates() []formatPredicate {
	formats := ProfileStringFormats()
	return []formatPredicate{
		{formats.Date, validDate},
		{formats.Time, validTime},
		{formats.DateTime, validDateTime},
		{formats.UUID, uuidExpression.MatchString},
		{formats.JSONPointer, validJSONPointer},
		{formats.RelativeJSONPointer, validRelativeJSONPointer},
	}
}

var (
	dateExpression = regexp.MustCompile(`\A[0-9]{4}-[0-9]{2}-[0-9]{2}\z`)
	timeExpression = regexp.MustCompile(`\A([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.[0-9]+)?([Zz]|([+-])([0-9]{2}):([0-9]{2}))\z`)
	uuidExpression = regexp.MustCompile(`\A[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}\z`)
)

func validDate(value string) bool {
	if !dateExpression.MatchString(value) {
		return false
	}
	_, err := time.Parse(time.DateOnly, value)
	return err == nil
}

const (
	hoursPerDay      = 24
	minutesPerHour   = int(time.Hour / time.Minute)
	secondsPerMinute = int(time.Minute / time.Second)
	minutesPerDay    = hoursPerDay * minutesPerHour
)

const (
	_ = iota
	timeHourGroup
	timeMinuteGroup
	timeSecondGroup
	_
	timeOffsetSignGroup
	timeOffsetHourGroup
	timeOffsetMinuteGroup
)

func validTime(value string) bool {
	parts := timeExpression.FindStringSubmatch(value)
	if parts == nil {
		return false
	}
	hour, minute, second := clockPart(parts[timeHourGroup]), clockPart(parts[timeMinuteGroup]), clockPart(parts[timeSecondGroup])
	if hour >= hoursPerDay || minute >= minutesPerHour || second > secondsPerMinute {
		return false
	}
	utcMinute := hour*minutesPerHour + minute
	if parts[timeOffsetSignGroup] != "" {
		offsetHour, offsetMinute := clockPart(parts[timeOffsetHourGroup]), clockPart(parts[timeOffsetMinuteGroup])
		if offsetHour >= hoursPerDay || offsetMinute >= minutesPerHour {
			return false
		}
		offset := offsetHour*minutesPerHour + offsetMinute
		if parts[timeOffsetSignGroup] == "-" {
			utcMinute += offset
		} else {
			utcMinute -= offset
		}
		utcMinute = (utcMinute + minutesPerDay) % minutesPerDay
	}
	// RFC 3339 permits leap seconds at the end of a UTC day, including offsets.
	return second < secondsPerMinute || utcMinute == minutesPerDay-1
}

func clockPart(digits string) int {
	return int(digits[0]-'0')*10 + int(digits[1]-'0')
}

func validDateTime(value string) bool {
	const dateLength = len(time.DateOnly)
	return len(value) > dateLength &&
		(value[dateLength] == 'T' || value[dateLength] == 't') &&
		validDate(value[:dateLength]) && validTime(value[dateLength+1:])
}

func validJSONPointer(value string) bool {
	if value != "" && !strings.HasPrefix(value, "/") {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] != '~' {
			continue
		}
		index++
		if index == len(value) || value[index] != '0' && value[index] != '1' {
			return false
		}
	}
	return true
}

func validRelativeJSONPointer(value string) bool {
	index := 0
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		index++
	}
	if index == 0 || index > 1 && value[0] == '0' {
		return false
	}
	return value[index:] == "#" || validJSONPointer(value[index:])
}
