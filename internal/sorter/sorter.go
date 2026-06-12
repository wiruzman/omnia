package sorter

type Column string

const (
	SortName     Column = "name"
	SortPath     Column = "path"
	SortSize     Column = "size"
	SortCreated  Column = "created"
	SortModified Column = "modified"
)

type Direction string

const (
	Asc  Direction = "ASC"
	Desc Direction = "DESC"
)

type SortSpec struct {
	Column    Column
	Direction Direction
}

func (s SortSpec) SQLOrderBy() string {
	col := "name_lower"
	switch s.Column {
	case SortName:
		col = "name_lower"
	case SortPath:
		col = "path_lower"
	case SortSize:
		col = "size"
	case SortCreated:
		col = "created_at"
	case SortModified:
		col = "modified_at"
	}
	dir := "ASC"
	if s.Direction == Desc {
		dir = "DESC"
	}
	return col + " " + dir + ", path_lower ASC"
}

func (s SortSpec) ToggleDirection() SortSpec {
	if s.Direction == Asc {
		s.Direction = Desc
	} else {
		s.Direction = Asc
	}
	return s
}

func (s SortSpec) NextColumn() SortSpec {
	switch s.Column {
	case SortName:
		s.Column = SortPath
	case SortPath:
		s.Column = SortSize
	case SortSize:
		s.Column = SortCreated
	case SortCreated:
		s.Column = SortModified
	default:
		s.Column = SortName
	}
	return s
}
