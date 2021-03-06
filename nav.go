package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type linkState byte

const (
	notLink linkState = iota
	working
	broken
)

type file struct {
	os.FileInfo
	linkState linkState
	path      string
	count     int
}

type filesSortable struct {
	files []*file
	less  func(i, j int) bool
}

func (f filesSortable) Len() int           { return len(f.files) }
func (f filesSortable) Swap(i, j int)      { f.files[i], f.files[j] = f.files[j], f.files[i] }
func (f filesSortable) Less(i, j int) bool { return f.less(i, j) }

// TODO: Replace with `sort.SliceStable` once available
func sortFilesStable(files []*file, less func(i, j int) bool) {
	sort.Stable(filesSortable{files: files, less: less})
}

func readdir(path string) ([]*file, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	names, err := f.Readdirnames(-1)
	f.Close()

	fi := make([]*file, 0, len(names))
	for _, filename := range names {
		fpath := filepath.Join(path, filename)

		lstat, lerr := os.Lstat(fpath)
		if os.IsNotExist(lerr) {
			continue
		}
		if lerr != nil {
			return fi, lerr
		}

		var linkState linkState

		if lstat.Mode()&os.ModeSymlink != 0 {
			stat, serr := os.Stat(fpath)
			if serr == nil {
				linkState = working
				lstat = stat
			} else {
				linkState = broken
				log.Printf("getting link destination info: %s", serr)
			}
		}

		fi = append(fi, &file{
			FileInfo:  lstat,
			linkState: linkState,
			path:      fpath,
			count:     -1,
		})
	}

	return fi, err
}

type dir struct {
	loading  bool      // directory is loading from disk
	loadTime time.Time // current loading or last load time
	ind      int       // index of current entry in fi
	pos      int       // position of current entry in ui
	path     string    // full path of directory
	fi       []*file   // displayed files in directory including or excluding hidden ones
	all      []*file   // all files in directory including hidden ones (same array as fi)
}

func newDir(path string) *dir {
	time := time.Now()

	fi, err := readdir(path)
	if err != nil {
		log.Printf("reading directory: %s", err)
	}

	return &dir{
		loadTime: time,
		path:     path,
		fi:       fi,
		all:      fi,
	}
}

func (dir *dir) renew() {
	fi, err := readdir(dir.path)
	if err != nil {
		log.Printf("reading directory: %s", err)
	}

	dir.fi = fi
	dir.all = fi
}

func (dir *dir) sort() {
	dir.fi = dir.all

	switch gOpts.sortby {
	case "natural":
		sortFilesStable(dir.fi, func(i, j int) bool {
			return naturalLess(strings.ToLower(dir.fi[i].Name()), strings.ToLower(dir.fi[j].Name()))
		})
	case "name":
		sortFilesStable(dir.fi, func(i, j int) bool {
			return strings.ToLower(dir.fi[i].Name()) < strings.ToLower(dir.fi[j].Name())
		})
	case "size":
		sortFilesStable(dir.fi, func(i, j int) bool {
			return dir.fi[i].Size() < dir.fi[j].Size()
		})
	case "time":
		sortFilesStable(dir.fi, func(i, j int) bool {
			return dir.fi[i].ModTime().Before(dir.fi[j].ModTime())
		})
	default:
		log.Printf("unknown sorting type: %s", gOpts.sortby)
	}

	if gOpts.reverse {
		for i, j := 0, len(dir.fi)-1; i < j; i, j = i+1, j-1 {
			dir.fi[i], dir.fi[j] = dir.fi[j], dir.fi[i]
		}
	}

	if gOpts.dirfirst {
		sortFilesStable(dir.fi, func(i, j int) bool {
			if dir.fi[i].IsDir() == dir.fi[j].IsDir() {
				return i < j
			}
			return dir.fi[i].IsDir()
		})
	}

	// when hidden option is disabled, we move hidden files to the
	// beginning of our file list and then set the beginning of displayed
	// files to the first non-hidden file in the list
	if !gOpts.hidden {
		sortFilesStable(dir.fi, func(i, j int) bool {
			if dir.fi[i].Name()[0] == '.' && dir.fi[j].Name()[0] == '.' {
				return i < j
			}
			return dir.fi[i].Name()[0] == '.'
		})
		for i, f := range dir.fi {
			if f.Name()[0] != '.' {
				dir.fi = dir.fi[i:]
				return
			}
		}
		dir.fi = dir.fi[len(dir.fi):]
	}
}

func (dir *dir) name() string {
	if len(dir.fi) == 0 {
		return ""
	}
	return dir.fi[dir.ind].Name()
}

func (dir *dir) find(name string, height int) {
	if len(dir.fi) == 0 {
		dir.ind, dir.pos = 0, 0
		return
	}

	dir.ind = min(dir.ind, len(dir.fi)-1)

	if dir.fi[dir.ind].Name() != name {
		for i, f := range dir.fi {
			if f.Name() == name {
				dir.ind = i
				break
			}
		}
	}

	edge := min(gOpts.scrolloff, len(dir.fi)-dir.ind-1)
	dir.pos = min(dir.ind, height-edge-1)
}

type nav struct {
	dirs     []*dir
	dirChan  chan *dir
	regChan  chan *reg
	dirCache map[string]*dir
	regCache map[string]*reg
	saves    map[string]bool
	marks    map[string]int
	markInd  int
	height   int
	search   string
}

func newNav(height int) *nav {
	wd, err := os.Getwd()
	if err != nil {
		log.Printf("getting current directory: %s", err)
	}

	nav := &nav{
		dirChan:  make(chan *dir),
		regChan:  make(chan *reg),
		dirCache: make(map[string]*dir),
		regCache: make(map[string]*reg),
		marks:    make(map[string]int),
		saves:    make(map[string]bool),
		markInd:  0,
		height:   height,
	}

	nav.getDirs(wd)

	return nav
}

func (nav *nav) position() {
	path := nav.currDir().path
	for i := len(nav.dirs) - 2; i >= 0; i-- {
		nav.dirs[i].find(filepath.Base(path), nav.height)
		path = filepath.Dir(path)
	}
}

func (nav *nav) getDirs(wd string) {
	var dirs []*dir

	for curr, base := wd, ""; !isRoot(base); curr, base = filepath.Dir(curr), filepath.Base(curr) {
		dir := nav.loadDir(curr)
		dir.find(base, nav.height)
		dirs = append(dirs, dir)
	}

	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}

	nav.dirs = dirs
}

func (nav *nav) renew(height int) {
	nav.dirCache = make(map[string]*dir)
	for _, d := range nav.dirs {
		nav.dirCache[d.path] = d
	}

	nav.height = height
	for _, d := range nav.dirs {
		go func(d *dir) {
			s, err := os.Stat(d.path)
			if err != nil {
				log.Printf("getting directory info: %s", err)
			}
			if d.loadTime.After(s.ModTime()) {
				return
			}
			d.loadTime = time.Now()
			nd := newDir(d.path)
			nd.sort()
			nav.dirChan <- nd
		}(d)
	}

	for m := range nav.marks {
		if _, err := os.Stat(m); os.IsNotExist(err) {
			delete(nav.marks, m)
		}
	}
	if len(nav.marks) == 0 {
		nav.markInd = 0
	}
}

func (nav *nav) reload() {
	nav.dirCache = make(map[string]*dir)
	nav.regCache = make(map[string]*reg)

	wd, err := os.Getwd()
	if err != nil {
		log.Printf("getting current directory: %s", err)
	}

	curr, err := nav.currFile()
	nav.getDirs(wd)
	if err == nil {
		last := nav.dirs[len(nav.dirs)-1]
		last.fi = append(last.fi, curr)
	}
}

func (nav *nav) loadDir(path string) *dir {
	d, ok := nav.dirCache[path]
	if !ok {
		go func() {
			d := newDir(path)
			d.sort()
			d.ind, d.pos = 0, 0
			nav.dirChan <- d
		}()
		d := &dir{loading: true, path: path}
		nav.dirCache[path] = d
		return d
	}
	return d
}

func (nav *nav) preview() {
	curr, err := nav.currFile()
	if err != nil {
		return
	}

	var reader io.Reader

	if len(gOpts.previewer) != 0 {
		cmd := exec.Command(gOpts.previewer, curr.path, strconv.Itoa(nav.height))

		out, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("previewing file: %s", err)
		}

		if err := cmd.Start(); err != nil {
			log.Printf("previewing file: %s", err)
		}

		defer cmd.Wait()
		defer out.Close()
		reader = out
	} else {
		f, err := os.Open(curr.path)
		if err != nil {
			log.Printf("opening file: %s", err)
		}

		defer f.Close()
		reader = f
	}

	reg := &reg{path: curr.path}

	buf := bufio.NewScanner(reader)

	for i := 0; i < nav.height && buf.Scan(); i++ {
		for _, r := range buf.Text() {
			if r == 0 {
				reg.lines = []string{"\033[1mbinary\033[0m"}
				nav.regChan <- reg
				return
			}
		}
		reg.lines = append(reg.lines, buf.Text())
	}

	if buf.Err() != nil {
		log.Printf("loading file: %s", buf.Err())
	}

	nav.regChan <- reg
}

func (nav *nav) loadReg(ui *ui, path string) *reg {
	r, ok := nav.regCache[path]
	if !ok {
		go nav.preview()
		r := &reg{path: path, lines: []string{"\033[1mloading...\033[0m"}}
		nav.regCache[path] = r
		return r
	}
	return r
}

func (nav *nav) sort() {
	for _, d := range nav.dirs {
		name := d.name()
		d.sort()
		d.find(name, nav.height)
	}
}

func (nav *nav) up(dist int) {
	dir := nav.currDir()

	if dir.ind == 0 {
		return
	}

	dir.ind -= dist
	dir.ind = max(0, dir.ind)

	dir.pos -= dist
	edge := min(gOpts.scrolloff, dir.ind)
	dir.pos = max(dir.pos, edge)
}

func (nav *nav) down(dist int) {
	dir := nav.currDir()

	maxind := len(dir.fi) - 1

	if dir.ind >= maxind {
		return
	}

	dir.ind += dist
	dir.ind = min(maxind, dir.ind)

	dir.pos += dist
	edge := min(gOpts.scrolloff, maxind-dir.ind)

	// use a smaller value when the height is even and scrolloff is maxed
	// in order to stay at the same row as much as possible while up/down
	edge = min(edge, nav.height/2+nav.height%2-1)

	dir.pos = min(dir.pos, nav.height-edge-1)
	dir.pos = min(dir.pos, maxind)
}

func (nav *nav) updir() error {
	if len(nav.dirs) <= 1 {
		return nil
	}

	dir := nav.currDir()

	nav.dirs = nav.dirs[:len(nav.dirs)-1]

	if err := os.Chdir(filepath.Dir(dir.path)); err != nil {
		return fmt.Errorf("updir: %s", err)
	}

	return nil
}

func (nav *nav) open() error {
	curr, err := nav.currFile()
	if err != nil {
		return fmt.Errorf("open: %s", err)
	}

	path := curr.path

	dir := nav.loadDir(path)

	nav.dirs = append(nav.dirs, dir)

	if err := os.Chdir(path); err != nil {
		return fmt.Errorf("open: %s", err)
	}

	return nil
}

func (nav *nav) top() {
	dir := nav.currDir()

	dir.ind = 0
	dir.pos = 0
}

func (nav *nav) bot() {
	dir := nav.currDir()

	dir.ind = len(dir.fi) - 1
	dir.pos = min(dir.ind, nav.height-1)
}

func (nav *nav) toggleMark(path string) {
	if _, ok := nav.marks[path]; ok {
		delete(nav.marks, path)
		if len(nav.marks) == 0 {
			nav.markInd = 0
		}
	} else {
		nav.marks[path] = nav.markInd
		nav.markInd = nav.markInd + 1
	}
}

func (nav *nav) toggle() {
	curr, err := nav.currFile()
	if err != nil {
		return
	}

	nav.toggleMark(curr.path)

	nav.down(1)
}

func (nav *nav) invert() {
	last := nav.currDir()
	for _, f := range last.fi {
		path := filepath.Join(last.path, f.Name())
		nav.toggleMark(path)
	}
}

func (nav *nav) unmark() {
	nav.marks = make(map[string]int)
	nav.markInd = 0
}

func (nav *nav) save(copy bool) error {
	if len(nav.marks) == 0 {
		curr, err := nav.currFile()
		if err != nil {
			return errors.New("no file selected")
		}

		if err := saveFiles([]string{curr.path}, copy); err != nil {
			return err
		}

		nav.saves = make(map[string]bool)
		nav.saves[curr.path] = copy
	} else {
		marks := nav.currMarks()

		if err := saveFiles(marks, copy); err != nil {
			return err
		}

		nav.saves = make(map[string]bool)
		for f := range nav.marks {
			nav.saves[f] = copy
		}
	}

	return nil
}

func (nav *nav) put() error {
	list, copy, err := loadFiles()
	if err != nil {
		return err
	}

	if len(list) == 0 {
		return errors.New("no file in yank/delete buffer")
	}

	dir := nav.currDir()

	cmd := putCommand(list, dir, copy)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("putting files: %s", err)
	}

	if err := saveFiles(nil, false); err != nil {
		return fmt.Errorf("clearing yank/delete buffer: %s", err)
	}

	return nil
}

func (nav *nav) sync() error {
	list, copy, err := loadFiles()
	if err != nil {
		return err
	}

	nav.saves = make(map[string]bool)
	for _, f := range list {
		nav.saves[f] = copy
	}

	return nil
}

func (nav *nav) cd(wd string) error {
	wd = strings.Replace(wd, "~", gUser.HomeDir, -1)
	wd = filepath.Clean(wd)

	if !filepath.IsAbs(wd) {
		wd = filepath.Join(nav.currDir().path, wd)
	}

	if err := os.Chdir(wd); err != nil {
		return fmt.Errorf("cd: %s", err)
	}

	nav.getDirs(wd)

	return nil
}

func (nav *nav) find(path string) error {
	lstat, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("select: %s", err)
	}

	dir := filepath.Dir(path)

	if err := nav.cd(dir); err != nil {
		return fmt.Errorf("select: %s", err)
	}

	base := filepath.Base(path)

	last := nav.dirs[len(nav.dirs)-1]
	if last.loading {
		last.fi = append(last.fi, &file{FileInfo: lstat})
	} else {
		last.find(base, nav.height)
	}

	return nil
}

func match(pattern, name string) (matched bool, err error) {
	if gOpts.ignorecase {
		lpattern := strings.ToLower(pattern)
		if !gOpts.smartcase || lpattern == pattern {
			pattern = lpattern
			name = strings.ToLower(name)
		}
	}
	if gOpts.globsearch {
		return filepath.Match(pattern, name)
	}
	return strings.Contains(name, pattern), nil
}

func (nav *nav) searchNext() error {
	last := nav.currDir()
	for i := last.ind + 1; i < len(last.fi); i++ {
		matched, err := match(nav.search, last.fi[i].Name())
		if err != nil {
			return err
		}
		if matched {
			nav.down(i - last.ind)
			return nil
		}
	}
	if gOpts.wrapscan {
		for i := 0; i < last.ind; i++ {
			matched, err := match(nav.search, last.fi[i].Name())
			if err != nil {
				return err
			}
			if matched {
				nav.up(last.ind - i)
				return nil
			}
		}
	}
	return nil
}

func (nav *nav) searchPrev() error {
	last := nav.currDir()
	for i := last.ind - 1; i >= 0; i-- {
		matched, err := match(nav.search, last.fi[i].Name())
		if err != nil {
			return err
		}
		if matched {
			nav.up(last.ind - i)
			return nil
		}
	}
	if gOpts.wrapscan {
		for i := len(last.fi) - 1; i > last.ind; i-- {
			matched, err := match(nav.search, last.fi[i].Name())
			if err != nil {
				return err
			}
			if matched {
				nav.down(i - last.ind)
				return nil
			}
		}
	}
	return nil
}

func (nav *nav) currDir() *dir {
	return nav.dirs[len(nav.dirs)-1]
}

func (nav *nav) currFile() (*file, error) {
	last := nav.dirs[len(nav.dirs)-1]

	if len(last.fi) == 0 {
		return nil, fmt.Errorf("empty directory")
	}
	return last.fi[last.ind], nil
}

type indexedMarks struct {
	paths   []string
	indices []int
}

func (m indexedMarks) Len() int { return len(m.paths) }

func (m indexedMarks) Swap(i, j int) {
	m.paths[i], m.paths[j] = m.paths[j], m.paths[i]
	m.indices[i], m.indices[j] = m.indices[j], m.indices[i]
}

func (m indexedMarks) Less(i, j int) bool { return m.indices[i] < m.indices[j] }

func (nav *nav) currMarks() []string {
	paths := make([]string, 0, len(nav.marks))
	indices := make([]int, 0, len(nav.marks))
	for path, index := range nav.marks {
		paths = append(paths, path)
		indices = append(indices, index)
	}
	sort.Sort(indexedMarks{paths: paths, indices: indices})
	return paths
}
