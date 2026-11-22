package runner

import (
	json "encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

type LaunchRequest struct {
	AttemptRef
	Command            string            `json:"command"`
	ExecutableDigest   framevalue.Digest `json:"executableDigest"`
	Args               []string          `json:"args,omitempty"`
	Env                []string          `json:"env"`
	Directory          string            `json:"directory,omitempty"`
	Configuration      framevalue.Digest `json:"configuration"`
	CredentialRevision string            `json:"credentialRevision,omitempty"`
}

// LaunchResult distinguishes this invocation's undispatched request from an
// original runner's durable observation. An undispatched retry says nothing
// about whether an earlier invocation of the same request was accepted.
type LaunchResult struct {
	Observation
	undispatchedRequest *framevalue.Digest
}

func (result LaunchResult) NotDispatched(request LaunchRequest) bool {
	if result.Observation != (Observation{}) || result.undispatchedRequest == nil {
		return false
	}
	digest, err := request.invocationDigest()
	return err == nil && digest == *result.undispatchedRequest
}

// notDispatched is this invocation's own proof that request never left it.
func (request LaunchRequest) notDispatched() LaunchResult {
	digest, err := request.invocationDigest()
	if err != nil {
		return LaunchResult{}
	}
	return LaunchResult{undispatchedRequest: &digest}
}

// invocationDigest binds a nondispatch result to this exact in-memory request,
// including its environment. It is never persisted; durableIdentity is.
func (request LaunchRequest) invocationDigest() (framevalue.Digest, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return framevalue.Digest{}, err
	}
	return framevalue.Hash(data), nil
}

func (request LaunchRequest) Validate() error {
	if request.AttemptRef.Validate() != nil || !filepath.IsAbs(request.Command) || !validProgramText(request.Command) || request.ExecutableDigest.Validate() != nil || request.Configuration.Validate() != nil {
		return fmt.Errorf("%w: runner requires exact launch identity", frame.ErrInvalid)
	}
	if request.Directory != "" && (!filepath.IsAbs(request.Directory) || !validProgramText(request.Directory)) {
		return fmt.Errorf("%w: runner working directory must be absolute utf-8 text without nul", frame.ErrInvalid)
	}
	if request.CredentialRevision != "" && !frame.ValidName(request.CredentialRevision) {
		return fmt.Errorf("%w: runner credential revision", frame.ErrInvalid)
	}
	for _, argument := range request.Args {
		if !validProgramText(argument) {
			return fmt.Errorf("%w: runner arguments must be utf-8 text without nul", frame.ErrInvalid)
		}
	}
	names := map[string]bool{}
	for _, entry := range request.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || !validProgramText(entry) || names[key] {
			return fmt.Errorf("%w: runner environment requires unique names and utf-8 text without nul", frame.ErrInvalid)
		}
		names[key] = true
	}
	return nil
}

func validProgramText(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsRune(text, '\x00')
}

// durableIdentity excludes credential values. While the child is live, its
// original injected environment is checked directly on replay.
func (request LaunchRequest) durableIdentity() (framevalue.Digest, error) {
	request.Env = nil
	data, err := json.Marshal(request)
	if err != nil {
		return framevalue.Digest{}, err
	}
	return framevalue.Hash(data), nil
}
