package storiface

// SectorFileStat describes a sector file, or cache directory, as present on a
// storage path. For directories Files maps the base names of the regular files
// inside to their sizes; for regular files Size is the file size.
type SectorFileStat struct {
	IsDir bool
	Size  int64
	Files map[string]int64
}
