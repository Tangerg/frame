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
    parser.add_argument("--version", default="v0.0.0", help="version to verify")
    args = parser.parse_args()
    if not re.fullmatch(r"v0\.\d+\.\d+", args.version):
        parser.error("expected a v0 semantic version")
    root = Path(__file__).resolve().parents[1]
    commit = git(root, "rev-parse", "HEAD")
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
  "testing"
  frame "{module}"
  "{module}/value"
)
func TestPlugin(t *testing.T) {{
  host, err := frame.NewHost(t.Context(), frame.HostConfig{{}})
  if err != nil {{t.Fatal(err)}}
  service, err := frame.NewContract[string]("editor.title","1")
  if err != nil {{t.Fatal(err)}}
  if err=host.Register(service); err!=nil {{t.Fatal(err)}}
  output := frame.Export("title", service)
  unit, err := frame.DefineUnit(frame.UnitSpec{{ID:"outline",Exports:frame.Exports(output),
      Prepare:func(s *frame.Setup)error{{return output.Bind(s,"Frame")}}}})
  if err != nil {{t.Fatal(err)}}
  plugin, err := frame.Define(frame.Definition{{Revision:value.Hash([]byte("outline-v1")), Units:[]frame.Unit{{unit}}}})
  if err != nil {{t.Fatal(err)}}
  installed, err := host.Install(t.Context(),frame.InstallationConfig{{ID:"outline",Plugin:plugin,Enabled:true}})
  if err != nil {{t.Fatal(err)}}
  root:=frame.ExportRef{{InstallationID:installed.ID,Release:installed.Release,UnitID:"outline",Export:"title",Contract:service.Ref()}}
  plan,err:=host.Plan(t.Context(),root);if err!=nil {{t.Fatal(err)}}
  grant,err:=host.Admission().OpenScope(t.Context(),"document");if err!=nil {{t.Fatal(err)}}
  use,err:=host.Acquire(t.Context(),plan,frame.UseConfig{{Owner:frame.NewLifetime(t.Context()),Scope:grant}});if err!=nil {{t.Fatal(err)}}
  err=frame.BorrowExport(t.Context(),use,root,func(ctx context.Context,title string)error{{
      if title!="Frame" {{t.Fatalf("title=%q",title)}};return nil
  }});if err!=nil {{t.Fatal(err)}}
  if _,err=use.Close(t.Context());err!=nil {{t.Fatal(err)}}
  if _,err=host.Close(t.Context());err!=nil {{t.Fatal(err)}}
}}
''')
        cached_proxy = os.environ.get("FRAME_DEPENDENCY_PROXY", os.environ.get("GOPROXY", "https://proxy.golang.org"))
        env = dict(os.environ, GOWORK="off", GONOPROXY="none", GONOSUMDB=module, GOMODCACHE=str(temp / "modules"),
                   GOPROXY=proxy.as_uri() + "," + cached_proxy)
        subprocess.run(["go", "test", "-mod=mod", "-count=1", "."], cwd=consumer, env=env, check=True)
        print(f"Verified {module}@{args.version} from {commit[:12]} with an external host.")


if __name__ == "__main__":
    main()
