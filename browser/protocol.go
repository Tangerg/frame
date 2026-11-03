// Package browser defines the Go-owned browser execution protocol. A Runtime
// materializes approved plans; these messages never delegate provider selection.
package browser

import (
	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

const Protocol = "io.github.tangerg.frame/control/1"

type Authority struct {
	ID    string `json:"id"`
	Epoch string `json:"epoch"`
}

type Realm struct {
	ID    string `json:"id"`
	Epoch string `json:"epoch"`
}

type Execution struct {
	ID         string `json:"id"`
	Generation string `json:"generation"`
}

type Instance struct {
	ID            string        `json:"id"`
	Generation    string        `json:"generation"`
	Source        frame.UnitRef `json:"source"`
	Configuration value.Digest  `json:"configuration"`
}

type LocalExport struct {
	InstanceID string            `json:"instanceId"`
	Name       string            `json:"name"`
	Contract   frame.ContractRef `json:"contract"`
}

type RemoteImport struct {
	Source frame.ExportRef `json:"source"`
	Token  string          `json:"token"`
}

type ImportKind string

const (
	LocalImportKind  ImportKind = "local"
	RemoteImportKind ImportKind = "remote"
	AbsentImportKind ImportKind = "absent"
)

type Import struct {
	Slot     string            `json:"slot"`
	Contract frame.ContractRef `json:"contract"`
	Kind     ImportKind        `json:"kind"`
	Local    *LocalExport      `json:"local,omitempty"`
	Remote   *RemoteImport     `json:"remote,omitempty"`
}

type Artifact struct {
	Path   string       `json:"path"`
	Digest value.Digest `json:"digest"`
}

type PlannedUnit struct {
	Instance          Instance                  `json:"instance"`
	ConfigurationJSON string                    `json:"configurationJson"`
	Profile           frame.Profile             `json:"profile"`
	Module            Artifact                  `json:"module"`
	Imports           []Import                  `json:"imports"`
	Exports           []frame.ExportDeclaration `json:"exports"`
}

type LocalPlan struct {
	Protocol        string        `json:"protocol"`
	ApprovalToken   string        `json:"approvalToken"`
	Authority       Authority     `json:"authority"`
	Realm           Realm         `json:"realm"`
	Execution       Execution     `json:"execution"`
	ScopeID         string        `json:"scopeId"`
	ScopeGeneration string        `json:"scopeGeneration"`
	Units           []PlannedUnit `json:"units"`
}

type PlanAdmissionDecision string

const (
	PlanAdmitted        PlanAdmissionDecision = "admitted"
	PlanCleanupRequired PlanAdmissionDecision = "cleanupRequired"
	PlanNotAdmitted     PlanAdmissionDecision = "notAdmitted"
	PlanSettled         PlanAdmissionDecision = "settled"
)

// An uncertain admission keeps its original local owner. Only the exact
// original attempt can establish whether that owner owes an execution proof.
type PlanAdmission struct {
	Authority Authority             `json:"authority"`
	Realm     Realm                 `json:"realm"`
	Execution Execution             `json:"execution"`
	Decision  PlanAdmissionDecision `json:"decision"`
}

type Fence struct {
	ScopeID         string `json:"scopeId"`
	ScopeGeneration string `json:"scopeGeneration"`
}

// Poll is a complete pending projection, not an event log. A missed response
// cannot lose a plan or fence; their original owners remove settled entries.
type Poll struct {
	Authority Authority   `json:"authority"`
	Realm     Realm       `json:"realm"`
	Revision  string      `json:"revision"`
	Plans     []LocalPlan `json:"plans"`
	Fences    []Fence     `json:"fences"`
}

type ObjectRef struct {
	Export   LocalExport `json:"export"`
	ObjectID string      `json:"objectId"`
}

type ReadyReport struct {
	OperationID   string                 `json:"operationId"`
	Authority     Authority              `json:"authority"`
	Realm         Realm                  `json:"realm"`
	Execution     Execution              `json:"execution"`
	Objects       []ObjectRef            `json:"objects"`
	Contributions []PreparedContribution `json:"contributions"`
}

type PreparedContribution struct {
	Object       ObjectRef `json:"object"`
	MetadataJSON string    `json:"metadataJson"`
}

type UseGrant struct {
	OperationID     string      `json:"operationId"`
	ParentUseID     string      `json:"parentUseId,omitempty"`
	ID              string      `json:"id"`
	ApprovalToken   string      `json:"approvalToken"`
	Authority       Authority   `json:"authority"`
	Realm           Realm       `json:"realm"`
	Execution       Execution   `json:"execution"`
	ScopeID         string      `json:"scopeId"`
	ScopeGeneration string      `json:"scopeGeneration"`
	Objects         []ObjectRef `json:"objects"`
}

type Candidate struct {
	PublicationID string          `json:"publicationId"`
	Source        frame.ExportRef `json:"source"`
	MetadataJSON  string          `json:"metadataJson"`
	Token         string          `json:"token"`
}

type PointSnapshot struct {
	Point      frame.ContractRef `json:"point"`
	Revision   string            `json:"revision"`
	Candidates []Candidate       `json:"candidates"`
}

type SnapshotRequest struct {
	OperationID string   `json:"operationId"`
	Points      []string `json:"points"`
}

type Snapshot struct {
	ID        string          `json:"id"`
	Authority Authority       `json:"authority"`
	Points    []PointSnapshot `json:"points"`
}

type Selection struct {
	Point      frame.ContractRef `json:"point"`
	Candidates []string          `json:"candidates"`
	Rule       string            `json:"rule"`
}

type AcquireRequest struct {
	OperationID string      `json:"operationId"`
	SnapshotID  string      `json:"snapshotId"`
	Execution   Execution   `json:"execution"`
	Selections  []Selection `json:"selections"`
	ParentUseID string      `json:"parentUseId,omitempty"`
}

type PublicationRequest struct {
	OperationID  string    `json:"operationId"`
	Authority    Authority `json:"authority"`
	Realm        Realm     `json:"realm"`
	Execution    Execution `json:"execution"`
	Object       ObjectRef `json:"object"`
	MetadataJSON string    `json:"metadataJson"`
}

type Publication struct {
	ID          string    `json:"id"`
	OperationID string    `json:"operationId"`
	Revision    string    `json:"revision"`
	Object      ObjectRef `json:"object"`
}

type Decision string

const (
	Accepted Decision = "accepted"
	Rejected Decision = "rejected"
	Unknown  Decision = "unknown"
)

type PublicationResult struct {
	Decision    Decision     `json:"decision"`
	Publication *Publication `json:"publication,omitempty"`
	Problem     string       `json:"problem,omitempty"`
}

type Withdrawal struct {
	Authority     Authority `json:"authority"`
	PublicationID string    `json:"publicationId"`
}

type RouteGrant struct {
	ID       string      `json:"id"`
	Use      UseGrant    `json:"use"`
	Source   LocalExport `json:"source"`
	Receiver LocalExport `json:"receiver"`
}

type ReleaseStatus string

const (
	Released    ReleaseStatus = "released"
	Unconfirmed ReleaseStatus = "unconfirmed"
)

type CloseProof struct {
	CloseID         string         `json:"closeId"`
	ProofID         string         `json:"proofId"`
	Authority       Authority      `json:"authority"`
	Realm           Realm          `json:"realm"`
	Execution       Execution      `json:"execution"`
	ScopeID         string         `json:"scopeId"`
	ScopeGeneration string         `json:"scopeGeneration"`
	ScopeKind       CloseScopeKind `json:"scopeKind"`
	UseID           string         `json:"useId,omitempty"`
	PublicationID   string         `json:"publicationId,omitempty"`
	Instances       []string       `json:"instances"`
	Status          ReleaseStatus  `json:"status"`
}

type ProofAcknowledgement struct {
	CloseID   string    `json:"closeId"`
	ProofID   string    `json:"proofId"`
	Recorded  bool      `json:"recorded"`
	Authority Authority `json:"authority"`
}

type ReconnectRequest struct {
	Authority  Authority   `json:"authority"`
	Realm      Realm       `json:"realm"`
	Executions []Execution `json:"executions"`
}

type ReconnectMode string

const (
	Revalidated ReconnectMode = "revalidated"
	CleanupOnly ReconnectMode = "cleanupOnly"
)

type ReconnectResult struct {
	Authority  Authority              `json:"authority"`
	Realm      Realm                  `json:"realm"`
	Mode       ReconnectMode          `json:"mode"`
	Executions []RevalidatedExecution `json:"executions"`
}

type RevalidatedExecution struct {
	Execution       Execution `json:"execution"`
	ScopeID         string    `json:"scopeId"`
	ScopeGeneration string    `json:"scopeGeneration"`
}

type CloseScopeKind string

const (
	ExecutionScope   CloseScopeKind = "execution"
	UseScope         CloseScopeKind = "use"
	PublicationScope CloseScopeKind = "publication"
)

func (ImportKind) Values() []string {
	return []string{string(LocalImportKind), string(RemoteImportKind), string(AbsentImportKind)}
}
func (Decision) Values() []string {
	return []string{string(Accepted), string(Rejected), string(Unknown)}
}
func (PlanAdmissionDecision) Values() []string {
	return []string{string(PlanAdmitted), string(PlanCleanupRequired), string(PlanNotAdmitted), string(PlanSettled)}
}
func (ReleaseStatus) Values() []string { return []string{string(Released), string(Unconfirmed)} }
func (ReconnectMode) Values() []string { return []string{string(Revalidated), string(CleanupOnly)} }
func (CloseScopeKind) Values() []string {
	return []string{string(ExecutionScope), string(UseScope), string(PublicationScope)}
}
