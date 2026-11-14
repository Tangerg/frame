export function taskView(id, state = "completed") {
  const source = { path: "hello.md", revision: "a".repeat(64) };
  return {
    id,
    source,
    binding: {
      roots: [],
      unitRoots: [],
      units: [],
      selection: "b".repeat(64),
    },
    state,
    runEpoch: "1",
    submission: { store: "test-store", owner: "test-owner", sequence: "1" },
    ...(state === "completed"
      ? {
          result: {
            converted: { source, html: "<p>kept</p>", title: "Kept" },
            diagnostics: [],
          },
        }
      : {}),
  };
}
