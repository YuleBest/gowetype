package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
)

// 只读 SQLite 解析器。
//
// 为什么要自己写：Android 上没有 sqlite3 命令行，而用 cgo 版的 SQLite 驱动就没法
// 交叉编译到手机。这里只实现"读"需要的那部分——表 b-tree 遍历、溢出页、记录解码，
// 再加上 WAL 叠加。不打开数据库文件本身，所以微信输入法正在运行时也能读。
//
// 用到的格式（都是 SQLite 官方文档里的）：
//
//	文件头 100 字节      page size / 保留区 / 文本编码
//	page 1               前 100 字节是文件头，之后才是 b-tree 页头
//	b-tree 页头          类型(2/5/10/13) | 单元格数 | 单元格指针数组
//	表叶子页单元格        varint 负载长度 | varint rowid | 负载(可能溢出到溢出页)
//	记录                  varint 头部长度 | 若干 serial type | 依次排列的值

const sqliteHeader = "SQLite format 3\x00"

// b-tree 页类型
const (
	pageInteriorIndex = 2
	pageInteriorTable = 5
	pageLeafIndex     = 10
	pageLeafTable     = 13
)

type sqliteDB struct {
	path     string
	pages    map[uint32][]byte
	pageSize int
	reserved int
	encoding int // 1=UTF-8 2=UTF-16le 3=UTF-16be
}

// openSQLite 读入整个数据库（含 WAL 叠加）到内存。
func openSQLite(path string) (*sqliteDB, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 100 || string(raw[:16]) != sqliteHeader {
		return nil, errors.New("不是 SQLite 数据库")
	}
	ps := int(binary.BigEndian.Uint16(raw[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps&(ps-1) != 0 {
		return nil, fmt.Errorf("page size 不合法: %d", ps)
	}
	db := &sqliteDB{
		path:     path,
		pages:    map[uint32][]byte{},
		pageSize: ps,
		reserved: int(raw[20]),
		encoding: int(binary.BigEndian.Uint32(raw[56:60])),
	}
	if db.encoding == 0 {
		db.encoding = 1
	}
	for i := 0; i+ps <= len(raw); i += ps {
		db.pages[uint32(i/ps+1)] = raw[i : i+ps]
	}
	// WAL 里的页比主库新，叠加在最后
	db.applyWAL(path + "-wal")
	return db, nil
}

// applyWAL 把 WAL 里已提交的帧叠加到内存页上。
//
// WAL 结构：32 字节文件头 + 若干帧，每帧 = 24 字节帧头 + 一页数据。
// 帧头里的 salt 必须和文件头一致，校验和是跨帧累加的（从文件头前 24 字节开始）。
// 只采纳到"最后一个完整提交"为止的帧——最后一帧的 dbSize 字段非 0 表示一次提交。
//
// 这里刻意不去改数据库文件（不做 checkpoint），纯读。
func (db *sqliteDB) applyWAL(walPath string) {
	data, err := os.ReadFile(walPath)
	if err != nil || len(data) < 32 {
		return
	}
	magic := binary.BigEndian.Uint32(data[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return
	}
	// magic 最低位是 SQLite 说的 bigEndCksum：为 0 表示校验和按本机字节序
	// （ARM/x86 上就是小端），为 1 表示反过来按大端。
	le := magic&1 == 0
	if int(binary.BigEndian.Uint32(data[8:12])) != db.pageSize {
		return
	}
	salt1 := binary.BigEndian.Uint32(data[16:20])
	salt2 := binary.BigEndian.Uint32(data[20:24])

	s1, s2 := walChecksum(0, 0, data[:24], le)
	if s1 != binary.BigEndian.Uint32(data[24:28]) ||
		s2 != binary.BigEndian.Uint32(data[28:32]) {
		return // 文件头校验不过，整份 WAL 作废
	}

	frameSize := 24 + db.pageSize
	type frame struct {
		pgno uint32
		data []byte
	}
	var frames []frame
	committed := 0

	for pos := 32; pos+frameSize <= len(data); pos += frameSize {
		fh := data[pos : pos+24]
		if binary.BigEndian.Uint32(fh[8:12]) != salt1 ||
			binary.BigEndian.Uint32(fh[12:16]) != salt2 {
			break
		}
		n1, n2 := walChecksum(s1, s2, fh[:8], le)
		n1, n2 = walChecksum(n1, n2, data[pos+24:pos+frameSize], le)
		if n1 != binary.BigEndian.Uint32(fh[16:20]) ||
			n2 != binary.BigEndian.Uint32(fh[20:24]) {
			break // 校验失败，后面都不要了
		}
		s1, s2 = n1, n2
		frames = append(frames, frame{
			pgno: binary.BigEndian.Uint32(fh[0:4]),
			data: data[pos+24 : pos+frameSize],
		})
		if binary.BigEndian.Uint32(fh[4:8]) != 0 {
			committed = len(frames) // 一次完整提交
		}
	}

	for _, f := range frames[:committed] {
		if f.pgno == 0 {
			continue
		}
		db.pages[f.pgno] = f.data
	}
}

// walChecksum 是 SQLite WAL 的滚动校验和。data 长度必须是 8 的倍数。
func walChecksum(s1, s2 uint32, data []byte, le bool) (uint32, uint32) {
	for i := 0; i+8 <= len(data); i += 8 {
		var x0, x1 uint32
		if le {
			x0 = binary.LittleEndian.Uint32(data[i:])
			x1 = binary.LittleEndian.Uint32(data[i+4:])
		} else {
			x0 = binary.BigEndian.Uint32(data[i:])
			x1 = binary.BigEndian.Uint32(data[i+4:])
		}
		s1 += x0 + s2
		s2 += x1 + s1
	}
	return s1, s2
}

// readVarint 读 SQLite 的变长整数：大端、每字节 7 位，最多 9 字节（第 9 字节给满 8 位）。
// 返回 (值, 消耗字节数)。
func readVarint(b []byte) (int64, int) {
	var v uint64
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, 0
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return int64(v), i + 1
		}
	}
	if len(b) < 9 {
		return 0, 0
	}
	return int64(v<<8 | uint64(b[8])), 9
}

func (db *sqliteDB) usable() int { return db.pageSize - db.reserved }

// walkTable 深度优先遍历一个表 b-tree，对每个叶子单元格回调 (rowid, 记录负载)。
func (db *sqliteDB) walkTable(root uint32, fn func(rowid int64, payload []byte) error) error {
	return db.walkTablePage(root, fn, 0)
}

func (db *sqliteDB) walkTablePage(pgno uint32, fn func(int64, []byte) error, depth int) error {
	if depth > 64 {
		return errors.New("b-tree 太深")
	}
	pg, ok := db.pages[pgno]
	if !ok {
		return fmt.Errorf("第 %d 页不存在", pgno)
	}
	hdr := 0
	if pgno == 1 {
		hdr = 100 // page 1 前面是 100 字节文件头
	}
	if hdr+12 > len(pg) {
		return errors.New("页头越界")
	}
	typ := pg[hdr]
	nCells := int(binary.BigEndian.Uint16(pg[hdr+3 : hdr+5]))
	// 单元格指针数组的位置：叶子页在页头之后（偏移 8），内部页要多跳过
	// 4 字节的最右子指针（偏移 12）。
	ptr := hdr + 8
	if typ == pageInteriorTable || typ == pageInteriorIndex {
		ptr = hdr + 12
	}
	if ptr+2*nCells > len(pg) {
		return errors.New("单元格指针数组越界")
	}

	switch typ {
	case pageInteriorTable:
		for i := 0; i < nCells; i++ {
			off := int(binary.BigEndian.Uint16(pg[ptr+2*i:]))
			if off+4 > len(pg) {
				return errors.New("内部单元格越界")
			}
			if err := db.walkTablePage(binary.BigEndian.Uint32(pg[off:]), fn, depth+1); err != nil {
				return err
			}
		}
		right := binary.BigEndian.Uint32(pg[hdr+8 : hdr+12])
		return db.walkTablePage(right, fn, depth+1)

	case pageLeafTable:
		for i := 0; i < nCells; i++ {
			off := int(binary.BigEndian.Uint16(pg[ptr+2*i:]))
			payload, rowid, err := db.leafCell(pg, off)
			if err != nil {
				return err
			}
			if err := fn(rowid, payload); err != nil {
				return err
			}
		}
		return nil

	default:
		return fmt.Errorf("第 %d 页不是表页（类型 %d）", pgno, typ)
	}
}

// leafCell 读一个表叶子页单元格，必要时把溢出页拼回来。
func (db *sqliteDB) leafCell(pg []byte, off int) ([]byte, int64, error) {
	if off >= len(pg) {
		return nil, 0, errors.New("单元格偏移越界")
	}
	plen, n := readVarint(pg[off:])
	if n == 0 {
		return nil, 0, errors.New("负载长度 varint 坏了")
	}
	off += n
	rowid, n := readVarint(pg[off:])
	if n == 0 {
		return nil, 0, errors.New("rowid varint 坏了")
	}
	off += n

	u := db.usable()
	maxLocal := u - 35
	if int(plen) <= maxLocal {
		if off+int(plen) > len(pg) {
			return nil, 0, errors.New("负载越界")
		}
		return pg[off : off+int(plen)], rowid, nil
	}

	// 溢出：SQLite 规定页面本地保留 K 字节，其余进溢出页
	m := ((u - 12) * 32 / 255) - 23
	k := m + (int(plen)-m)%(u-4)
	local := m
	if k <= maxLocal {
		local = k
	}
	if off+local+4 > len(pg) {
		return nil, 0, errors.New("溢出单元格越界")
	}
	out := make([]byte, 0, int(plen))
	out = append(out, pg[off:off+local]...)
	next := binary.BigEndian.Uint32(pg[off+local:])
	remain := int(plen) - local
	for next != 0 && remain > 0 {
		op, ok := db.pages[next]
		if !ok || len(op) < u {
			return nil, 0, fmt.Errorf("溢出页 %d 缺失", next)
		}
		chunk := u - 4
		if chunk > remain {
			chunk = remain
		}
		out = append(out, op[4:4+chunk]...)
		remain -= chunk
		next = binary.BigEndian.Uint32(op[0:4])
	}
	if remain != 0 {
		return nil, 0, errors.New("溢出链断了")
	}
	return out, rowid, nil
}

// decodeRecord 把记录负载解成一行值。
// 记录 = varint 头部长度 | 若干 serial type | 依次排列的值。
func (db *sqliteDB) decodeRecord(payload []byte) ([]any, error) {
	hdrSize, n := readVarint(payload)
	if n == 0 || hdrSize < int64(n) || hdrSize > int64(len(payload)) {
		return nil, errors.New("记录头长度不合法")
	}
	var serials []int64
	for p := n; p < int(hdrSize); {
		st, m := readVarint(payload[p:])
		if m == 0 {
			return nil, errors.New("serial type varint 坏了")
		}
		p += m
		serials = append(serials, st)
	}

	body := int(hdrSize)
	out := make([]any, 0, len(serials))
	for _, st := range serials {
		v, size, err := db.decodeValue(st, payload[body:])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		body += size
	}
	return out, nil
}

func (db *sqliteDB) decodeValue(st int64, b []byte) (any, int, error) {
	switch {
	case st == 0:
		return nil, 0, nil
	case st >= 1 && st <= 6:
		size := [...]int{0, 1, 2, 3, 4, 6, 8}[st]
		if len(b) < size {
			return nil, 0, errors.New("整数值越界")
		}
		var v int64
		for _, x := range b[:size] {
			v = v<<8 | int64(x)
		}
		if size < 8 && b[0]&0x80 != 0 { // 符号扩展
			v -= int64(1) << (8 * size)
		}
		return v, size, nil
	case st == 7:
		if len(b) < 8 {
			return nil, 0, errors.New("浮点值越界")
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), 8, nil
	case st == 8:
		return int64(0), 0, nil
	case st == 9:
		return int64(1), 0, nil
	case st >= 12 && st%2 == 0:
		size := int((st - 12) / 2)
		if len(b) < size {
			return nil, 0, errors.New("blob 越界")
		}
		return append([]byte(nil), b[:size]...), size, nil
	case st >= 13:
		size := int((st - 13) / 2)
		if len(b) < size {
			return nil, 0, errors.New("文本越界")
		}
		return db.decodeText(b[:size]), size, nil
	default:
		return nil, 0, fmt.Errorf("不认识的 serial type %d", st)
	}
}

func (db *sqliteDB) decodeText(b []byte) string {
	switch db.encoding {
	case 2: // UTF-16 LE
		r := make([]rune, 0, len(b)/2)
		for i := 0; i+2 <= len(b); i += 2 {
			r = append(r, rune(binary.LittleEndian.Uint16(b[i:])))
		}
		return string(r)
	case 3: // UTF-16 BE
		r := make([]rune, 0, len(b)/2)
		for i := 0; i+2 <= len(b); i += 2 {
			r = append(r, rune(binary.BigEndian.Uint16(b[i:])))
		}
		return string(r)
	default:
		return string(b)
	}
}

// table 描述 sqlite_master 里的一条表记录。
type sqliteTable struct {
	Name     string
	RootPage uint32
	SQL      string
}

// tableSchema 是从建表语句里抠出来的列信息。
type tableSchema struct {
	Columns []string
	// RowidAlias 是哪一列被声明成了 INTEGER PRIMARY KEY（-1 表示没有）。
	//
	// 这种列是 rowid 的别名：SQLite 在记录里给它存的是 NULL，真值在 b-tree 的
	// rowid 上。读的时候必须拿 rowid 补回去，否则这一列全是空。
	RowidAlias int
}

// tables 列出所有表。sqlite_master 的根页固定是第 1 页，列是
// type, name, tbl_name, rootpage, sql。
func (db *sqliteDB) tables() ([]sqliteTable, error) {
	var out []sqliteTable
	err := db.walkTable(1, func(_ int64, payload []byte) error {
		vals, err := db.decodeRecord(payload)
		if err != nil {
			return nil // 跳过解不开的记录
		}
		if len(vals) < 5 || asString(vals[0]) != "table" {
			return nil
		}
		root, _ := vals[3].(int64)
		out = append(out, sqliteTable{
			Name:     asString(vals[1]),
			RootPage: uint32(root),
			SQL:      asString(vals[4]),
		})
		return nil
	})
	return out, err
}

// rows 遍历一张表，返回列名和每一行。
func (db *sqliteDB) rows(t sqliteTable) ([]string, [][]any, error) {
	sch := parseTableSQL(t.SQL)
	var all [][]any
	err := db.walkTable(t.RootPage, func(rowid int64, payload []byte) error {
		vals, err := db.decodeRecord(payload)
		if err != nil {
			return nil
		}
		// INTEGER PRIMARY KEY 列在记录里是 NULL，用 rowid 补回真值
		if i := sch.RowidAlias; i >= 0 && i < len(vals) && vals[i] == nil {
			vals[i] = rowid
		}
		all = append(all, vals)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	cols := sch.Columns
	if len(cols) == 0 {
		// 没有 SQL（比如虚拟表）就退回用列数编号
		max := 0
		for _, r := range all {
			if len(r) > max {
				max = len(r)
			}
		}
		for i := 0; i < max; i++ {
			cols = append(cols, fmt.Sprintf("col%d", i+1))
		}
	}
	return cols, all, nil
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

// parseTableSQL 从建表语句里抠出列名，并找出 INTEGER PRIMARY KEY 那一列。
// 只处理普通的 CREATE TABLE，够用；抠不出来就返回空，由调用方兜底。
func parseTableSQL(sql string) tableSchema {
	sch := tableSchema{RowidAlias: -1}
	i := strings.IndexByte(sql, '(')
	j := strings.LastIndexByte(sql, ')')
	if i < 0 || j <= i {
		return sch
	}
	// WITHOUT ROWID 的表没有 rowid，也就没有别名列
	withoutRowid := strings.Contains(strings.ToUpper(sql[j:]), "WITHOUT ROWID")

	body := sql[i+1 : j]
	depth := 0
	start := 0
	for k := 0; k <= len(body); k++ {
		if k == len(body) || (body[k] == ',' && depth == 0) {
			part := strings.TrimSpace(body[start:k])
			start = k + 1
			if part == "" {
				continue
			}
			name := part
			cut := strings.IndexAny(part, " \t(")
			if cut < 0 {
				cut = len(part)
			}
			name = strings.Trim(part[:cut], "`\"[]")
			up := strings.ToUpper(name)
			if up == "PRIMARY" || up == "UNIQUE" || up == "CHECK" ||
				up == "FOREIGN" || up == "CONSTRAINT" {
				continue // 表级约束，不是列
			}
			rest := strings.ToUpper(part[cut:])
			if !withoutRowid && strings.Contains(rest, "PRIMARY KEY") {
				// 只有类型恰好是 INTEGER 才是 rowid 别名
				// （`INTEGER PRIMARY KEY DESC` 是例外，极少见，这里不区分）
				if f := strings.Fields(rest); len(f) > 0 && f[0] == "INTEGER" {
					sch.RowidAlias = len(sch.Columns)
				}
			}
			sch.Columns = append(sch.Columns, name)
			continue
		}
		switch body[k] {
		case '(':
			depth++
		case ')':
			depth--
		}
	}
	return sch
}
