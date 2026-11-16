"""Build a module proxy from the committed tree and verify a real external consumer."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


def git(root: Path, *arguments: str) -> str:
    return subprocess.check_output(["git", *arguments], cwd=root, text=True).strip()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--proxy", type=Path, help="retain unpublished module distribution here")
    parser.add_argument("--version", help="Go module version to verify (defaults to the committed SDK version)")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    commit = git(root, "rev-parse", "HEAD")
    if args.version is None:
        args.version = "v" + json.loads(git(root, "show", commit + ":sdk/package.json"))["version"]
    if not re.fullmatch(r"v0\.\d+\.\d+", args.version):
        parser.error("expected a v0 semantic version")
    manifest = git(root, "show", commit + ":go.mod") + "\n"
    module = re.search(r"^module ([^\s]+)$", manifest, re.M).group(1)
    toolchain = re.search(r"^go ([\d.]+)$", manifest, re.M).group(1)
    escaped = re.sub(r"[A-Z]", lambda match: "!" + match[0].lower(), module)

    with tempfile.TemporaryDirectory(prefix="frame-go-consumer-") as tmp:
        temp = Path(tmp)
        proxy = args.proxy.resolve() if args.proxy else temp / "proxy"
        versions = proxy / escaped / "@v"
        versions.mkdir(parents=True, exist_ok=True)
        prefix = versions / args.version
        Path(str(prefix) + ".mod").write_text(manifest)
        Path(str(prefix) + ".info").write_text(json.dumps({
            "Version": args.version,
            "Time": git(root, "show", "-s", "--format=%cI", commit),
        }))
        (versions / "list").write_text(args.version + "\n")
        subprocess.run(["git", "archive", "--format=zip", "--prefix=" + module + "@" + args.version + "/",
                        "--output=" + str(prefix) + ".zip", commit], cwd=root, check=True)

        consumer = temp / "consumer"
        consumer.mkdir()
        (consumer / "go.mod").write_text(f"module example.org/editor\n\ngo {toolchain}\n\nrequire {module} {args.version}\n")
        (consumer / "consumer_test.go").write_text(f'''package editor
import (
  "context"
  "errors"
  "os"
  "path/filepath"
  "strings"
  "testing"
  frame "{module}"
  framehost "{module}/host"
  "{module}/browser"
  "{module}/contractgen"
  "{module}/distribution"
  "{module}/process/runner"
  "{module}/sqlite"
  "{module}/value"
)
var _ func(*browser.Bridge,context.Context,string,browser.ReadyReport)(browser.DecisionResult,error) = (*browser.Bridge).Ready
var _ func(*browser.Bridge,context.Context,string,browser.Withdrawal)(browser.DecisionResult,error) = (*browser.Bridge).Withdraw
var _ interface{{JSONSchemaEnum() []string}} = browser.Decision("")
var _ func([]byte)([]value.StringEnumeration,error) = value.ProjectStringEnums
var _ func(framehost.Host,context.Context,...framehost.ContractRef)(framehost.Snapshot,error) = framehost.Host.Snapshot
var _ func(framehost.Host,context.Context,framehost.BindingPlan,framehost.UseConfig)(framehost.Use,error) = framehost.Host.Acquire
var _ func(framehost.Host,context.Context,[]*framehost.Plugin)error = framehost.Host.Restore
var _ func(sqlite.Store,context.Context,framehost.UseApproval)(framehost.CommitState,error) = sqlite.Store.ApproveUse
var _ func(sqlite.Store,context.Context,sqlite.Attempt)(framehost.CommitState,error) = sqlite.Store.RecordAttempt
var _ func(sqlite.Store,context.Context,sqlite.Attempt,sqlite.ExitProof)error = sqlite.Store.SettleAttempt
var _ func(*browser.Bridge,context.Context,string,string,framehost.BindingPlan,string)(browser.UseGrant,error) = (*browser.Bridge).AcquirePlan
var _ func(frame.Lifetime,error)frame.Close = frame.Lifetime.BeginClose
var _ func(*frame.CloseCauses,...error)error = (*frame.CloseCauses).Join
var _ func(context.Context)error = frame.ContextError
var _ func(any)bool = value.IsNil
var _ interface{{Validate() error}} = runner.AttemptRef{{}}
var _ interface{{Validate() error}} = runner.ExitReceipt{{}}
var _ error = runner.ErrUnavailable
func TestContextAndValueBoundaries(t *testing.T) {{
  cause:=errors.New("original cancellation")
  ctx,cancel:=context.WithCancelCause(t.Context())
  cancel(cause)
  failure:=frame.ContextError(ctx)
  if !errors.Is(failure,context.Canceled)||!errors.Is(failure,cause) {{t.Fatalf("cancellation facts changed: %v",failure)}}
  var missing context.Context
  if !errors.Is(frame.ContextError(missing),frame.ErrInvalid) {{t.Fatal("nil context admitted")}}
  var absent *int
  if !value.IsNil(absent)||value.IsNil(new(int)) {{t.Fatal("typed nil admission changed")}}
}}
func TestStringEnumProjection(t *testing.T) {{
  projections,err:=value.ProjectStringEnums([]byte(`{{"type":"string","enum":["approved","denied"],"not":{{"const":"denied"}}}}`))
  if err!=nil {{t.Fatal(err)}}
  if len(projections)!=1||projections[0].Pointer!=""||len(projections[0].Values)!=1||projections[0].Values[0]!="approved" {{t.Fatalf("published projection changed: %#v",projections)}}
}}
func TestEnumAdmission(t *testing.T) {{
  descriptor,err:=value.SchemaFor[browser.Decision]();if err!=nil {{t.Fatal(err)}}
  schema,err:=value.AdmitSchema(descriptor);if err!=nil {{t.Fatal(err)}}
  for _,sample:=range []struct{{raw string; admitted bool}}{{{{`"accepted"`,true}},{{`"invented"`,false}}}} {{
    var decoded browser.Decision
    if got:=value.DecodeValue([]byte(sample.raw),&decoded,"decision")==nil;got!=sample.admitted {{t.Fatalf("Go enum admission=%v",got)}}
    if got:=schema.Validate([]byte(sample.raw))==nil;got!=sample.admitted {{t.Fatalf("published enum admission=%v",got)}}
  }}
}}
func TestCloseHandles(t *testing.T) {{
  owner:=frame.NewLifetime(context.Background())
  operation:=owner.BeginClose(frame.ErrClosed)
  original:=operation
  operation=frame.Close{{}}
  if _,err:=operation.Wait(t.Context());!errors.Is(err,frame.ErrInvalid) {{t.Fatalf("unissued close admitted: %v",err)}}
  outcome,err:=original.WaitReleased(t.Context());if err!=nil||!outcome.Released {{t.Fatalf("original close lost: %#v, %v",outcome,err)}}
  proof,err:=original.ReleaseProof();if err!=nil||proof.CloseID!=outcome.ID {{t.Fatalf("original proof changed: %#v, %v",proof,err)}}
  if current:=owner.BeginClose(frame.ErrClosed).Outcome();current.ID!=outcome.ID {{t.Fatal("close handle replacement changed the original operation")}}
}}
func TestCloseFailureComposition(t *testing.T) {{
  owner:=frame.NewLifetime(t.Context())
  causes:=owner.ObserveCloseCauses()
  original:=errors.New("original artifact cleanup failure")
  if _,err:=owner.Own(frame.Resource{{Name:"original",Close:func(context.Context)(frame.ReleaseState,error){{return frame.ResourceReleased,original}}}});err!=nil {{t.Fatal(err)}}
  outcome,err:=owner.Close(t.Context())
  combined:=causes.Join(err,outcome.Err())
  if !outcome.Released||!errors.Is(combined,original)||!errors.Is(combined,frame.ErrCloseFailed)||strings.Count(combined.Error(),original.Error())!=1 {{t.Fatalf("published close composition lost original facts: %v",combined)}}
}}
type reconstructionDriver struct{{ contract framehost.Contract[string] }}
func (reconstructionDriver) Profile() framehost.Profile {{ return "consumer.realm" }}
func (reconstructionDriver) RealmBound() bool {{ return true }}
func (d reconstructionDriver) Prepare(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {{
  return []framehost.ExportValue{{framehost.Provide("value", d.contract, string(setup.Realm()))}}, nil
}}
func TestCapturedRealmReconstruction(t *testing.T) {{
  descriptor := []byte(`{{"protocol":"artifact-reconstruction"}}`)
  contract, err := framehost.BindContract[string](framehost.ContractRef{{Name: "artifact.reconstruction", Version: "1", Kind: framehost.ServiceKind, Mode: framehost.PortableMode, Digest: value.Hash(descriptor)}}, descriptor)
  if err != nil {{ t.Fatal(err) }}
  driver := reconstructionDriver{{contract: contract}}
  host, err := framehost.New(t.Context(), framehost.Config{{Drivers: []framehost.Driver{{driver}}}})
  if err != nil {{ t.Fatal(err) }}
  defer host.Close(context.Background())
  if err := host.Register(contract); err != nil {{ t.Fatal(err) }}
  unit := framehost.Unit{{Declaration: framehost.UnitDeclaration{{ID: "main", Profile: driver.Profile(), Exports: []framehost.ExportDeclaration{{{{Name: "value", Contract: contract.Ref()}}}}}}}}
  plugin, err := framehost.Define(framehost.Definition{{Revision: value.Hash([]byte("artifact-reconstruction")), Units: []framehost.Unit{{unit}}}})
  if err != nil {{ t.Fatal(err) }}
  installed, err := host.Install(t.Context(), framehost.InstallationConfig{{ID: "original", Plugin: plugin, Enabled: true}})
  if err != nil {{ t.Fatal(err) }}
  if _, err := host.ActivateIn(t.Context(), "original-realm", installed.Unit("main").Export("value", contract.Ref())); err != nil {{ t.Fatal(err) }}
  for _, realm := range []framehost.RealmID{{"original-realm", "another-realm"}} {{
    snapshot, err := host.SnapshotIn(t.Context(), realm, contract.Ref())
    if err != nil {{ t.Fatal(err) }}
    members, err := snapshot.Candidates(contract.Ref())
    if err != nil {{ t.Fatal(err) }}
    selected, err := framehost.SelectRefs(snapshot, contract.Ref(), "original", members...)
    if err != nil {{ t.Fatal(err) }}
    plan, err := framehost.Capture(snapshot, selected)
    if err != nil {{ t.Fatal(err) }}
    _, err = plan.Snapshot()
    if realm == "original-realm" && err != nil || realm != "original-realm" && !errors.Is(err, frame.ErrInvalid) {{ t.Fatalf("artifact lost original Realm provenance: %s, %v", realm, err) }}
  }}
}}
func TestStartClaimsJoinOriginalRetirement(t *testing.T) {{
  host, err := framehost.New(t.Context(), framehost.Config{{}})
  if err != nil {{ t.Fatal(err) }}
  defer host.Close(context.Background())
  entered := make(chan struct{{}})
  var original frame.ResourceHandle
  unit := framehost.Unit{{
    Declaration: framehost.UnitDeclaration{{ID: "main", Profile: framehost.NativeGo}},
    Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {{
      var err error
      original, err = setup.Own(frame.Resource{{Name: "original execution", Close: func(context.Context) (frame.ReleaseState, error) {{
        close(entered)
        return frame.ResourceUnconfirmed, nil
      }}}})
      return nil, err
    }},
  }}
  plugin, err := framehost.Define(framehost.Definition{{Revision: value.Hash([]byte("artifact-start-claims")), Units: []framehost.Unit{{unit}}}})
  if err != nil {{ t.Fatal(err) }}
  installed, err := host.Install(t.Context(), framehost.InstallationConfig{{ID: "original", Plugin: plugin, Enabled: true}})
  if err != nil {{ t.Fatal(err) }}
  grant, err := host.Admission().OpenScope(t.Context(), "original-owner")
  if err != nil {{ t.Fatal(err) }}
  first, err := host.StartUnit(t.Context(), grant, installed.Unit("main"))
  if err != nil {{ t.Fatal(err) }}
  defer original.ConfirmReleased(nil)
  second, err := host.StartUnit(t.Context(), grant, installed.Unit("main"))
  if err != nil {{ t.Fatal(err) }}
  if err := host.RetireInstance(t.Context(), first.ID()); err != nil {{ t.Fatal(err) }}
  <-entered
  canceled, cancel := context.WithCancel(t.Context())
  cancel()
  for _, claim := range []framehost.RunningUnit{{first, second}} {{
    if err := claim.Stop(canceled); !errors.Is(err, context.Canceled) {{ t.Errorf("pending original exit was lost: %v", err) }}
  }}
  if err := original.ConfirmReleased(nil); err != nil {{ t.Fatal(err) }}
  if err := host.WaitInstanceRetired(t.Context(), first.ID()); err != nil {{ t.Fatal(err) }}
  for _, claim := range []framehost.RunningUnit{{first, second}} {{
    if err := claim.Stop(canceled); err != nil {{ t.Errorf("completed original exit was lost: %v", err) }}
  }}
}}
func TestPortableContractAndTypedCall(t *testing.T) {{
  method,err:=frame.NewTypedMethod[string,string](frame.MethodConfig{{Name:"replace",Kind:frame.Command}})
  if err!=nil {{t.Fatal(err)}}
  capability,err:=frame.NewCapability("editor.portable",method.Descriptor())
  if err!=nil {{t.Fatal(err)}}
  implementation,err:=method.ImplementCommand(func(_ context.Context,id,input string)(string,error){{
    if id!="original-command" {{t.Fatal("business identity changed")}}
    return input,nil
  }})
  if err!=nil {{t.Fatal(err)}}
  service,err:=frame.Implement(capability,implementation)
  if err!=nil {{t.Fatal(err)}}
  output,err:=method.Command(t.Context(),service,"original-command","original-input")
  if err!=nil||output!="original-input" {{t.Fatalf("typed call=%q: %v",output,err)}}
  contract,err:=framehost.NewPortableContract[frame.Service]("1",capability)
  if err!=nil {{t.Fatal(err)}}
  host,err:=framehost.New(t.Context(),framehost.Config{{}})
  if err!=nil {{t.Fatal(err)}}
  defer host.Close(t.Context())
  if err=host.Register(contract);err!=nil {{t.Fatal(err)}}
  retained,err:=host.Capability(contract.Ref())
  if err!=nil||retained.Identity()!=capability.Identity() {{t.Fatalf("admitted contract changed: %v",err)}}
  directory:=t.TempDir()
  source:=filepath.Join(directory,"capability.json")
  if err=os.WriteFile(source,contract.Descriptor(),0600);err!=nil {{t.Fatal(err)}}
  if err=contractgen.Generate(source,"",directory,contractgen.GoOutput{{Path:filepath.Join(directory,"types.go"),Package:"editor"}});err!=nil {{t.Fatal(err)}}
}}
func TestPlugin(t *testing.T) {{
  host, err := framehost.New(t.Context(), framehost.Config{{}})
  if err != nil {{t.Fatal(err)}}
  service, err := framehost.NewContract[string]("editor.title","1")
  if err != nil {{t.Fatal(err)}}
  if err=host.Register(service); err!=nil {{t.Fatal(err)}}
  output := framehost.Export("title", service)
  unit, err := framehost.DefineUnit(framehost.UnitSpec{{ID:"outline",Exports:framehost.Exports(output),
      Prepare:func(s framehost.Setup)error{{return output.Bind(s,"Frame")}}}})
  if err != nil {{t.Fatal(err)}}
  plugin, err := framehost.Define(framehost.Definition{{Revision:value.Hash([]byte("outline-v1")), Units:[]framehost.Unit{{unit}}}})
  if err != nil {{t.Fatal(err)}}
  installed, err := host.Install(t.Context(),framehost.InstallationConfig{{ID:"outline",Plugin:plugin,Enabled:true}})
  if err != nil {{t.Fatal(err)}}
  root:=framehost.ExportRef{{InstallationID:installed.ID,Release:installed.Release,UnitID:"outline",Export:"title",Contract:service.Ref()}}
  plan,err:=host.Plan(t.Context(),root);if err!=nil {{t.Fatal(err)}}
  grant,err:=host.Admission().OpenScope(t.Context(),"document");if err!=nil {{t.Fatal(err)}}
  use,err:=host.Acquire(t.Context(),plan,framehost.UseConfig{{Owner:frame.NewLifetime(t.Context()),Scope:grant}});if err!=nil {{t.Fatal(err)}}
  err=framehost.BorrowExport(t.Context(),use,root,func(ctx context.Context,title string)error{{
      if title!="Frame" {{t.Fatalf("title=%q",title)}};return nil
  }});if err!=nil {{t.Fatal(err)}}
  if _,err=use.Close(t.Context());err!=nil {{t.Fatal(err)}}
  if _,err=host.Close(t.Context());err!=nil {{t.Fatal(err)}}
}}
func TestDurableAuthority(t *testing.T) {{
  admission := framehost.NewAdmission()
  store,err:=sqlite.Open(t.Context(),sqlite.Config{{Path:filepath.Join(t.TempDir(),"frame.db"),Admission:admission}})
  if err!=nil {{t.Fatal(err)}}
  original:=store
  defer original.Close()
  host,err:=framehost.New(t.Context(),framehost.Config{{Authority:store}})
  if err!=nil {{t.Fatal(err)}}
  defer host.Close(t.Context())
  store=sqlite.Store{{}}
  if host.Durable()==framehost.Durable(store)||host.Durable()!=framehost.Durable(original) {{t.Fatal("caller replacement changed the original authority")}}
  if _,err=original.Owner("missing");err==nil {{t.Fatal("undeclared owner admitted")}}
}}
func TestRestorationUsesOriginalCommittedAuthority(t *testing.T) {{
  store,err:=sqlite.Open(t.Context(),sqlite.Config{{Path:filepath.Join(t.TempDir(),"restoration.db"),Admission:framehost.NewAdmission()}})
  if err!=nil {{t.Fatal(err)}}
  defer store.Close()
  host,err:=framehost.New(t.Context(),framehost.Config{{Authority:store}})
  if err!=nil {{t.Fatal(err)}}
  defer host.Close(context.Background())
  prepared:=make(chan string,1)
  plugin,err:=framehost.Define(framehost.Definition{{Revision:value.Hash([]byte("restoration-original-authority")),Units:[]framehost.Unit{{{{
    Declaration:framehost.UnitDeclaration{{ID:"main",Profile:framehost.NativeGo}},
    Setup:func(_ context.Context,setup framehost.Setup)([]framehost.ExportValue,error){{
      prepared<-string(setup.Configuration())
      return nil,nil
    }},
  }}}}}})
  if err!=nil {{t.Fatal(err)}}
  installed,err:=host.Install(t.Context(),framehost.InstallationConfig{{ID:"worker",Plugin:plugin,Configuration:[]byte("original"),Enabled:true}})
  if err!=nil {{t.Fatal(err)}}
  previous,err:=store.Installations(t.Context())
  if err!=nil {{t.Fatal(err)}}
  if err=host.Replace(t.Context(),installed.ID,plugin,[]byte("committed update"));err!=nil {{t.Fatal(err)}}
  if len(previous)!=1||string(previous[0].Configuration)!="original" {{t.Fatal("previous read did not retain the old configuration")}}
  if err=host.Restore(t.Context(),[]*framehost.Plugin{{plugin}});err!=nil {{t.Fatal(err)}}
  if _,err=host.ActivateUnit(t.Context(),"",installed.Unit("main"));err!=nil {{t.Fatal(err)}}
  if got:=<-prepared;got!="committed update" {{t.Fatalf("restoration executed superseded configuration: %q",got)}}
  current,found:=host.Installation(installed.ID)
  if !found||string(current.Configuration)!="committed update" {{t.Fatalf("restoration advanced a competing record: %+v",current)}}
}}
func TestDistributionAuthority(t *testing.T) {{
  store,err:=distribution.Open(distribution.Config{{
    Directory:t.TempDir(),Admission:framehost.NewAdmission(),
    Trust:func(context.Context,distribution.Source,distribution.Manifest,distribution.Lock)error{{return nil}},
    Retained:func(context.Context,value.Digest)(bool,error){{return false,nil}},
  }})
  if err!=nil {{t.Fatal(err)}}
  original:=store
  defer original.Close(context.Background())
  if _,err=distribution.NewResourceDriver(store);err!=nil {{t.Fatal(err)}}
  if err=store.Close(t.Context());err!=nil {{t.Fatal(err)}}
  if _,err=original.Path(value.Hash([]byte("release")));!errors.Is(err,frame.ErrClosed) {{t.Fatalf("copy reopened the original directory: %v",err)}}
  store=distribution.Store{{}}
  if _,err=distribution.NewResourceDriver(store);!errors.Is(err,frame.ErrInvalid) {{t.Fatalf("unissued directory admitted: %v",err)}}
  if err=store.Close(t.Context());err!=nil {{t.Fatal(err)}}
}}
func TestPreparedMetadata(t *testing.T) {{
  host,err:=framehost.New(t.Context(),framehost.Config{{}})
  if err!=nil {{t.Fatal(err)}}
  defer host.Close(t.Context())
  point,err:=framehost.NewPoint[string,string]("editor.metadata","1")
  if err!=nil {{t.Fatal(err)}}
  if err=host.Register(point);err!=nil {{t.Fatal(err)}}
  metadata,err:=host.PrepareMetadata(point.Ref(),[]byte(`"Original"`))
  if err!=nil {{t.Fatal(err)}}
  plugin,err:=framehost.Define(framehost.Definition{{Revision:value.Hash([]byte("metadata")),Units:[]framehost.Unit{{{{
    Declaration:framehost.UnitDeclaration{{ID:"main",Profile:framehost.NativeGo,Exports:[]framehost.ExportDeclaration{{{{Name:"entry",Contract:point.Ref()}}}}}},
    Setup:func(context.Context,framehost.Setup)([]framehost.ExportValue,error){{
      return []framehost.ExportValue{{{{Name:"entry",Contract:point.Ref(),Metadata:metadata,Value:"callable"}}}},nil
    }},
  }}}}}})
  if err!=nil {{t.Fatal(err)}}
  installed,err:=host.Install(t.Context(),framehost.InstallationConfig{{ID:"metadata",Plugin:plugin,Enabled:true}})
  if err!=nil {{t.Fatal(err)}}
  root:=framehost.ExportRef{{InstallationID:installed.ID,Release:installed.Release,UnitID:"main",Export:"entry",Contract:point.Ref()}}
  if _,err=host.Activate(t.Context(),root);err!=nil {{t.Fatal(err)}}
  snapshot,err:=host.Snapshot(t.Context(),point.Ref());if err!=nil {{t.Fatal(err)}}
  candidates,err:=snapshot.Candidates(point.Ref());if err!=nil||len(candidates)!=1 {{t.Fatalf("candidates=%d error=%v",len(candidates),err)}}
  encoded,err:=candidates[0].MetadataJSON();if err!=nil||string(encoded)!=metadata.JSON().String() {{t.Fatalf("metadata=%s error=%v",encoded,err)}}
  originalSnapshot:=snapshot
  selected,err:=framehost.SelectRefs(originalSnapshot,point.Ref(),"original",candidates[0]);if err!=nil {{t.Fatal(err)}}
  snapshot=framehost.Snapshot{{}}
  if _,err=snapshot.Candidates(point.Ref());!errors.Is(err,frame.ErrInvalid) {{t.Fatalf("unissued snapshot admitted: %v",err)}}
  plan,err:=framehost.Capture(originalSnapshot,selected);if err!=nil {{t.Fatal(err)}}
  originalPlan:=plan
  plan=framehost.BindingPlan{{}}
  if _,err=plan.Snapshot();!errors.Is(err,frame.ErrInvalid) {{t.Fatalf("unissued plan admitted: %v",err)}}
  owner:=frame.NewLifetime(t.Context())
  defer owner.Close(context.Background())
  scope,err:=host.Admission().OpenScope(t.Context(),"metadata-consumer");if err!=nil {{t.Fatal(err)}}
  use,err:=host.Acquire(t.Context(),originalPlan,framehost.UseConfig{{Owner:owner,Scope:scope}});if err!=nil {{t.Fatal(err)}}
  defer use.Close(context.Background())
  if err=framehost.BorrowExport(t.Context(),use,root,func(_ context.Context,value string)error{{
    if value!="callable" {{t.Fatalf("original implementation changed: %q",value)}};return nil
  }});err!=nil {{t.Fatal(err)}}
}}
''')
        cached_proxy = os.environ.get("FRAME_DEPENDENCY_PROXY", os.environ.get("GOPROXY", "https://proxy.golang.org"))
        env = dict(os.environ, GOWORK="off", GONOPROXY="none", GONOSUMDB=module, GOMODCACHE=str(temp / "modules"),
                   GOPROXY=proxy.as_uri() + "," + cached_proxy)
        authored = consumer / "native"
        authored.mkdir()
        source_directory = "examples/authoring/native/"
        for path in git(root, "ls-tree", "-r", "--name-only", commit, source_directory).splitlines():
            if path.endswith(".go") and not path.endswith("_test.go"):
                (authored / Path(path).name).write_text(git(root, "show", commit + ":" + path) + "\n")
        subprocess.run(["go", "test", "-mod=mod", "-count=1", "./..."], cwd=consumer, env=env, check=True)
        binaries = temp / "bin"
        command_env = dict(env, GOBIN=str(binaries))
        commands = ("frame", "frame-runner", "authorhost", "buildcheck", "document")
        subprocess.run(["go", "install", *(f"{module}/cmd/{name}@{args.version}" for name in commands)],
                       cwd=consumer, env=command_env, check=True)
        for name in commands:
            if not os.access(binaries / name, os.X_OK):
                raise RuntimeError(f"published command lost its executable name: {name}")
        rendered = subprocess.check_output([str(binaries / "buildcheck"), "-apply"], input="  TODO artifact  \n",
                                           text=True, cwd=consumer, env=command_env)
        json.loads(rendered)
        state = temp / "unopened-state"
        projection = temp / "api.ts"
        subprocess.run([str(binaries / "document"), "-command", "generate", "-out", str(projection),
                        "-state", str(state)], cwd=consumer, env=command_env, check=True)
        if state.exists() or not projection.is_file():
            raise RuntimeError("published document generator opened application state")
        print(f"Verified {module}@{args.version} from {commit[:12]} with an external host and installed commands.")


if __name__ == "__main__":
    main()
