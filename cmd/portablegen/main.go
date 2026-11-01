package main

import (
	"encoding/json"
	"log"
	"os"

	"github.com/Tangerg/frame/portable"
)

//go:generate go run . ../../protocol/process/portable-profile.generated.json

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: portablegen output")
	}
	profile := struct {
		Protocol string          `json:"protocol"`
		Limits   portable.Limits `json:"limits"`
	}{portable.Protocol, portable.ProfileLimits()}
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(os.Args[1], append(data, '\n'), 0644); err != nil {
		log.Fatal(err)
	}
}
