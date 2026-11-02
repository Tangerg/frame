package main

import (
	"context"
	"crypto/sha256"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/distribution"
	"github.com/Tangerg/frame/internal/localfile"
	"github.com/Tangerg/frame/value"
)

func boundedFile(path string, limit int64) ([]byte, error) {
	file, err := localfile.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, distribution.ErrCapacity
	}
	return data, err
}

func runPackage(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("frame package", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	source := flags.String("source", "", "release source directory; defaults to manifest directory")
	manifestPath := flags.String("manifest", "", "source manifest")
	destination := flags.String("out", "", "new archive path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *destination == "" || (*source == "" && *manifestPath == "") {
		return errors.New("provide --manifest or --source, and --out")
	}
	if *manifestPath == "" {
		*manifestPath = filepath.Join(*source, distribution.ManifestName)
	}
	if *source == "" {
		*source = filepath.Dir(*manifestPath)
	}
	data, err := boundedFile(*manifestPath, distribution.DefaultLimits().ManifestBytes)
	if err != nil {
		return err
	}
	var manifest distribution.Manifest
	if err = json.Unmarshal(data, &manifest, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(*destination), ".frame-package-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	hash := sha256.New()
	lock, err := distribution.Build(context.Background(), *source, manifest, io.MultiWriter(temp, hash))
	if err == nil {
		err = temp.Sync()
	}
	if err = errors.Join(err, temp.Close()); err != nil {
		return err
	}
	// A reader never sees a partial archive; a concurrent publisher cannot replace the winner.
	if err = os.Link(temp.Name(), *destination); err != nil {
		return err
	}
	return json.MarshalWrite(output, struct {
		ArchiveDigest value.Digest      `json:"archiveDigest"`
		Lock          distribution.Lock `json:"lock"`
	}{value.DigestOf([sha256.Size]byte(hash.Sum(nil))), lock})
}

type inspection struct {
	ArchiveDigest  value.Digest          `json:"archiveDigest"`
	ReleaseDigest  value.Digest          `json:"releaseDigest"`
	Manifest       distribution.Manifest `json:"manifest"`
	Integrity      string                `json:"integrity"`
	HostSupport    string                `json:"hostProfileSupport"`
	PublisherTrust string                `json:"publisherTrust"`
	Execution      string                `json:"execution"`
}

type publisherPolicy struct {
	Archives []value.Digest `json:"allowedArchiveDigests"`
}

func runInspect(args []string, output io.Writer) error {
	return inspectCommand(args, output, false)
}

func runVerify(args []string, output io.Writer) error {
	return inspectCommand(args, output, true)
}

func inspectCommand(args []string, output io.Writer, verify bool) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("provide an archive path followed by options")
	}
	archive := args[0]
	flags := flag.NewFlagSet("archive", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	format := flags.String("format", "json", "report format (json)")
	expected, policyPath := "", ""
	if verify {
		flags.StringVar(&expected, "digest", "", "expected archive digest")
		flags.StringVar(&policyPath, "policy", "", "publisher policy containing trusted archive digests")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *format != "json" {
		return errors.New("expected --format json and no extra arguments")
	}
	limits := distribution.DefaultLimits()
	file, err := localfile.OpenRegular(archive)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, limits.ArchiveBytes+1))
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	if size > limits.ArchiveBytes {
		return distribution.ErrCapacity
	}
	digest := value.DigestOf([sha256.Size]byte(hash.Sum(nil)))
	if expected != "" {
		pin, err := value.ParseDigest(expected)
		if err != nil {
			return err
		}
		if pin != digest {
			return distribution.ErrIntegrity
		}
	}
	release, err := distribution.VerifyArchive(context.Background(), archive, digest, limits)
	if err != nil {
		return err
	}
	result := inspection{ArchiveDigest: digest, ReleaseDigest: release.Digest, Manifest: release.Manifest, Integrity: "verified", PublisherTrust: "not_assessed", HostSupport: "not_assessed", Execution: "not_authorized"}
	if policyPath != "" {
		data, err := boundedFile(policyPath, limits.ManifestBytes)
		if err != nil {
			return err
		}
		var policy publisherPolicy
		if err = json.Unmarshal(data, &policy, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		if !slices.Contains(policy.Archives, digest) {
			return fmt.Errorf("%w: archive digest is not approved", distribution.ErrUntrusted)
		}
		result.PublisherTrust = "pinned_archive"
	}
	return json.MarshalWrite(output, result)
}

func diagnosticCode(err error) string {
	switch {
	case errors.Is(err, distribution.ErrUntrusted):
		return "untrusted"
	case errors.Is(err, distribution.ErrIntegrity), errors.Is(err, value.ErrInvalidDigest):
		return "integrity"
	case errors.Is(err, distribution.ErrCapacity):
		return "capacity"
	case errors.Is(err, os.ErrNotExist):
		return "not_found"
	case errors.Is(err, os.ErrExist):
		return "already_exists"
	case errors.Is(err, frame.ErrContractMismatch):
		return "contract_mismatch"
	}
	return "invalid_request"
}
