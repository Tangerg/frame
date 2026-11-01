package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/distribution"
	"github.com/Tangerg/frame/value"
)

func TestPackageAndVerifyPreserveTheExactArtifactAndRefusePartialOutput(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "theme.css"), []byte("body { color: blue }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := distribution.Manifest{
		SchemaVersion: 1, Package: "example.theme", Version: "1.0.0",
		Artifacts:    []distribution.Artifact{{Path: "theme.css"}},
		Units:        []distribution.Unit{{ID: "theme", Profile: frame.StaticResource, Resources: []string{"theme.css"}}},
		Dependencies: []distribution.LockedRelease{{Package: "example.dependency", Release: value.Hash([]byte("original dependency"))}},
	}
	writeManifest := func() {
		t.Helper()
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, distribution.ManifestName), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest()
	archive := filepath.Join(t.TempDir(), "theme.zip")
	var output bytes.Buffer
	if err := runPackage([]string{"--source", source, "--out", archive}, &output); err != nil {
		t.Fatal(err)
	}
	var report struct {
		ArchiveDigest value.Digest      `json:"archiveDigest"`
		Lock          distribution.Lock `json:"lock"`
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Lock.Dependencies) != 1 || report.Lock.Dependencies[0] != manifest.Dependencies[0] {
		t.Fatal("package command lost canonical manifest dependencies")
	}
	data, err := os.ReadFile(archive)
	if err != nil || report.ArchiveDigest != value.Hash(data) {
		t.Fatalf("archive receipt: %v, %v", report.ArchiveDigest, err)
	}
	output.Reset()
	if err := runVerify([]string{archive, "--digest", report.ArchiveDigest.String()}, &output); err != nil {
		t.Fatal(err)
	}
	var verified inspection
	if err := json.Unmarshal(output.Bytes(), &verified); err != nil || verified.ReleaseDigest != report.Lock.Release {
		t.Fatalf("verified identity: %v, %v", verified.ReleaseDigest, err)
	}
	if err := runPackage([]string{"--source", source, "--out", archive}, &output); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrote an existing artifact: %v", err)
	}
	if actual, err := os.ReadFile(archive); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("rejected publication changed the original archive")
	}
	if err := os.WriteFile(archive, append(data, 'x'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runVerify([]string{archive, "--digest", report.ArchiveDigest.String()}, &output); err == nil {
		t.Fatal("changed bytes retained the original archive identity")
	}
	manifest.Artifacts = append(manifest.Artifacts, distribution.Artifact{Path: "missing.css"})
	writeManifest()
	incomplete := filepath.Join(t.TempDir(), "incomplete.zip")
	if err := runPackage([]string{"--source", source, "--out", incomplete}, &output); err == nil {
		t.Fatal("missing declared artifact was accepted")
	}
	if _, err := os.Stat(incomplete); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed publication left an apparent archive: %v", err)
	}
}

func TestArchiveInspectionSeparatesIntegrityTrustAndExecution(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "theme.css"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := distribution.Manifest{SchemaVersion: distribution.ManifestSchemaVersion, Package: "theme", Version: "1", Artifacts: []distribution.Artifact{{Path: "theme.css"}}, Units: []distribution.Unit{{ID: "theme", Profile: frame.StaticResource, Resources: []string{"theme.css"}}}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(source, distribution.ManifestName)
	if err = os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "theme.zip")
	var output bytes.Buffer
	if err = runPackage([]string{"--manifest", input, "--out", archive}, &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err = runInspect([]string{archive}, &output); err != nil {
		t.Fatal(err)
	}
	var result inspection
	if err = json.Unmarshal(output.Bytes(), &result); err != nil || result.PublisherTrust != "not_assessed" || result.Execution != "not_authorized" {
		t.Fatal(result, err)
	}
	policy := filepath.Join(source, "trust.json")
	data, err = json.Marshal(publisherPolicy{Archives: []value.Digest{result.ArchiveDigest}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(policy, data, 0600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err = runVerify([]string{archive, "--policy", policy}, &output); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(output.Bytes(), &result); err != nil || result.PublisherTrust != "pinned_archive" {
		t.Fatal(result, err)
	}
	if err = os.WriteFile(policy, []byte(`{"allowedArchiveDigests":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = runVerify([]string{archive, "--policy", policy}, &output); !errors.Is(err, distribution.ErrUntrusted) {
		t.Fatal(err)
	}
}
