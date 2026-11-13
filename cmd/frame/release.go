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
	"github.com/Tangerg/frame/contractgen"
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
		return errors.Join(frame.ErrInvalid, err)
	}
	if flags.NArg() != 0 || *destination == "" || (*source == "" && *manifestPath == "") {
		return fmt.Errorf("%w: provide --manifest or --source, and --out", frame.ErrInvalid)
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
		return errors.Join(distribution.ErrInvalid, err)
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
	Integrity      integrityStatus       `json:"integrity"`
	HostSupport    hostSupportStatus     `json:"hostProfileSupport"`
	PublisherTrust publisherTrustStatus  `json:"publisherTrust"`
	Execution      executionStatus       `json:"execution"`
}

type integrityStatus string
type hostSupportStatus string
type publisherTrustStatus string
type executionStatus string
type diagnosticCode string

const (
	integrityVerified      integrityStatus      = "verified"
	hostSupportNotAssessed hostSupportStatus    = "not_assessed"
	publisherNotAssessed   publisherTrustStatus = "not_assessed"
	publisherPinnedArchive publisherTrustStatus = "pinned_archive"
	executionNotAuthorized executionStatus      = "not_authorized"

	diagnosticUntrusted        diagnosticCode = "untrusted"
	diagnosticIntegrity        diagnosticCode = "integrity"
	diagnosticCapacity         diagnosticCode = "capacity"
	diagnosticNotFound         diagnosticCode = "not_found"
	diagnosticAlreadyExists    diagnosticCode = "already_exists"
	diagnosticContractMismatch diagnosticCode = "contract_mismatch"
	diagnosticInvalidRequest   diagnosticCode = "invalid_request"
	diagnosticOperationFailed  diagnosticCode = "operation_failed"
)

type publisherPolicy struct {
	Archives []value.Digest `json:"allowedArchiveDigests"`
}

func runInspect(args []string, output io.Writer) error {
	options, err := parseArchiveOptions(args, nil)
	if err != nil {
		return err
	}
	return inspectArchive(output, options)
}

func runVerify(args []string, output io.Writer) error {
	options, err := parseArchiveOptions(args, func(flags *flag.FlagSet, options *archiveOptions) {
		flags.StringVar(&options.expected, "digest", "", "expected archive digest")
		flags.StringVar(&options.policy, "policy", "", "publisher policy containing trusted archive digests")
	})
	if err != nil {
		return err
	}
	return inspectArchive(output, options)
}

type archiveOptions struct {
	archive  string
	expected string
	policy   string
}

func parseArchiveOptions(args []string, register func(*flag.FlagSet, *archiveOptions)) (archiveOptions, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return archiveOptions{}, fmt.Errorf("%w: provide an archive path followed by options", frame.ErrInvalid)
	}
	options := archiveOptions{archive: args[0]}
	flags := flag.NewFlagSet("archive", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	format := flags.String("format", jsonFormat, "report format (json)")
	if register != nil {
		register(flags, &options)
	}
	if err := flags.Parse(args[1:]); err != nil {
		return archiveOptions{}, errors.Join(frame.ErrInvalid, err)
	}
	if flags.NArg() != 0 || *format != jsonFormat {
		return archiveOptions{}, fmt.Errorf("%w: expected --format json and no extra arguments", frame.ErrInvalid)
	}
	return options, nil
}

const jsonFormat = "json"

func inspectArchive(output io.Writer, options archiveOptions) error {
	limits := distribution.DefaultLimits()
	digest, err := archiveDigest(options.archive, limits)
	if err != nil {
		return err
	}
	if options.expected != "" {
		pin, err := value.ParseDigest(options.expected)
		if err != nil {
			return err
		}
		if pin != digest {
			return distribution.ErrIntegrity
		}
	}
	release, err := distribution.VerifyArchive(context.Background(), options.archive, digest, limits)
	if err != nil {
		return err
	}
	result := inspection{ArchiveDigest: digest, ReleaseDigest: release.Digest, Manifest: release.Manifest, Integrity: integrityVerified, PublisherTrust: publisherNotAssessed, HostSupport: hostSupportNotAssessed, Execution: executionNotAuthorized}
	if options.policy != "" {
		if err := checkPublisherPolicy(options.policy, digest, limits); err != nil {
			return err
		}
		result.PublisherTrust = publisherPinnedArchive
	}
	return json.MarshalWrite(output, result)
}

func archiveDigest(archive string, limits distribution.Limits) (value.Digest, error) {
	file, err := localfile.OpenRegular(archive)
	if err != nil {
		return value.Digest{}, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, limits.ArchiveBytes+1))
	if err = errors.Join(err, file.Close()); err != nil {
		return value.Digest{}, err
	}
	if size > limits.ArchiveBytes {
		return value.Digest{}, distribution.ErrCapacity
	}
	return value.DigestOf([sha256.Size]byte(hash.Sum(nil))), nil
}

func checkPublisherPolicy(path string, digest value.Digest, limits distribution.Limits) error {
	data, err := boundedFile(path, limits.ManifestBytes)
	if err != nil {
		return err
	}
	var policy publisherPolicy
	if err = json.Unmarshal(data, &policy, json.RejectUnknownMembers(true)); err != nil {
		return errors.Join(frame.ErrInvalid, err)
	}
	if !slices.Contains(policy.Archives, digest) {
		return fmt.Errorf("%w: archive digest is not approved", distribution.ErrUntrusted)
	}
	return nil
}

func classifyDiagnostic(err error) diagnosticCode {
	switch {
	case errors.Is(err, distribution.ErrUntrusted):
		return diagnosticUntrusted
	case errors.Is(err, distribution.ErrIntegrity), errors.Is(err, value.ErrInvalidDigest):
		return diagnosticIntegrity
	case errors.Is(err, distribution.ErrCapacity):
		return diagnosticCapacity
	case errors.Is(err, os.ErrNotExist):
		return diagnosticNotFound
	case errors.Is(err, os.ErrExist):
		return diagnosticAlreadyExists
	case errors.Is(err, frame.ErrContractMismatch):
		return diagnosticContractMismatch
	case errors.Is(err, frame.ErrInvalid), errors.Is(err, distribution.ErrInvalid), errors.Is(err, contractgen.ErrInvalid), errors.Is(err, os.ErrInvalid):
		return diagnosticInvalidRequest
	}
	return diagnosticOperationFailed
}
