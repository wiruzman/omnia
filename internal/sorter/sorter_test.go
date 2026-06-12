package sorter

import "testing"

func TestSortSpecSQLOrderBy(t *testing.T) {
	s := SortSpec{Column: SortSize, Direction: Desc}
	got := s.SQLOrderBy()
	want := "size DESC, path_lower ASC"
	if got != want {
		t.Fatalf("expected %q got %q", want, got)
	}
}

func TestSortCycleAndToggle(t *testing.T) {
	s := SortSpec{Column: SortName, Direction: Asc}
	s = s.NextColumn()
	if s.Column != SortPath {
		t.Fatalf("expected path got %s", s.Column)
	}
	s = s.ToggleDirection()
	if s.Direction != Desc {
		t.Fatalf("expected desc got %s", s.Direction)
	}
}
