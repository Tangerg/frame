package sqlite

import (
	"testing"
)

func TestReadScopeKeepsSQLitePragmaCaseInsensitivity(t *testing.T) {
	store, _, _ := testStore(t)
	for _, statement := range []string{
		`PRAGMA table_info(frame_meta)`,
		`PRAGMA TABLE_INFO(frame_meta)`,
		`PRAGMA TaBlE_InFo(frame_meta)`,
	} {
		if err := store.Read(t.Context(), func(reader Reader) error {
			rows, err := reader.QueryContext(t.Context(), statement)
			if err != nil {
				return err
			}
			return rows.Close()
		}); err != nil {
			t.Errorf("inspection %q changed with keyword spelling: %v", statement, err)
		}
	}
	for _, statement := range []string{
		`PRAGMA READ_UNCOMMITTED=1`,
		`PRAGMA WaL_CheCkPoInT(TRUNCATE)`,
		`PRAGMA foreign_Key_list(frame_meta)`,
	} {
		if err := store.Read(t.Context(), func(reader Reader) error {
			rows, err := reader.QueryContext(t.Context(), statement)
			if err != nil {
				return err
			}
			return rows.Close()
		}); err == nil {
			t.Errorf("case folding admitted connection mutation %q", statement)
		}
	}
}
