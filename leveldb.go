package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// 这个文件实现只读的 LevelDB 读取：直接解析磁盘上的文件，不打开数据库。
// 这样即使微信输入法正在运行、数据库被锁着，也能安全读取。
//
// 一个 LevelDB 目录里有两类数据文件：
//
//	*.ldb  SSTable，已落盘的、按 key 排序的不可变表
//	*.log  预写日志（WAL），最近写入但还没合并进 .ldb 的记录
//
// 读取时要先读 .ldb，再用 .log 覆盖，才能得到最新状态。

// sstMagic 是 LevelDB table footer 末尾的 8 字节魔数。
const sstMagic uint64 = 0xdb4775248b80fb57

type kv struct {
	key   []byte
	value []byte
}

// sstEntries 解析一个 .ldb 文件，按 key 序返回所有条目。
//
// 文件布局（从后往前看）：
//
//	[data block] ... [metaindex block] [index block] [footer 48B]
//
// footer 里存着 metaindex 和 index 两个 block 的位置（各是一对 varint 的
// offset/size），末尾 8 字节是魔数。
// index block 的每个条目是「分隔 key -> BlockHandle(offset,size)」，
// 顺着它就能把每个 data block 读出来。
func sstEntries(path string) ([]kv, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 48 {
		return nil, errors.New("文件太小")
	}
	footer := raw[len(raw)-48:]
	if binary.LittleEndian.Uint64(footer[40:]) != sstMagic {
		return nil, errors.New("footer 魔数不对")
	}

	p := 0
	var n int
	if _, n, err = uvarint(footer[p:]); err != nil {
		return nil, err
	}
	p += n
	if _, n, err = uvarint(footer[p:]); err != nil { // metaindex，用不到
		return nil, err
	}
	p += n
	ixOff, n, err := uvarint(footer[p:])
	if err != nil {
		return nil, err
	}
	p += n
	ixSize, _, err := uvarint(footer[p:])
	if err != nil {
		return nil, err
	}

	index, err := readBlock(raw, int(ixOff), int(ixSize))
	if err != nil {
		return nil, err
	}
	idxEntries, err := parseBlock(index)
	if err != nil {
		return nil, err
	}

	var out []kv
	for _, e := range idxEntries {
		// index 条目的 value 是 BlockHandle：varint offset + varint size
		off, n, err := uvarint(e.value)
		if err != nil {
			return nil, err
		}
		size, _, err := uvarint(e.value[n:])
		if err != nil {
			return nil, err
		}
		data, err := readBlock(raw, int(off), int(size))
		if err != nil {
			return nil, err
		}
		ents, err := parseBlock(data)
		if err != nil {
			return nil, err
		}
		out = append(out, ents...)
	}
	return out, nil
}

// readBlock 取一个 block 的原始内容并解压。
// BlockHandle 的 size 只算内容，后面还紧跟 5 字节 trailer：1 字节压缩类型 + 4 字节 crc32c。
func readBlock(raw []byte, off, size int) ([]byte, error) {
	if off < 0 || size < 0 || off+size+5 > len(raw) {
		return nil, errors.New("block 越界")
	}
	contents := raw[off : off+size]
	switch raw[off+size] {
	case 0:
		return contents, nil
	case 1:
		return decodeSnappy(contents)
	default:
		return nil, fmt.Errorf("不支持的压缩类型 %d", raw[off+size])
	}
}

// parseBlock 解析一个 block 里的所有 key/value。
//
// block 内容 = 若干条目 + restart 数组 + restart 数量(4B)。
// 条目用前缀压缩：shared / nonShared / valueLen 三个 varint，然后是
// key 的新增部分和 value。key = 上一条 key 的前 shared 字节 + 新增部分。
func parseBlock(data []byte) ([]kv, error) {
	if len(data) < 4 {
		return nil, nil
	}
	numRestarts := int(binary.LittleEndian.Uint32(data[len(data)-4:]))
	end := len(data) - 4 - 4*numRestarts
	if end < 0 || end > len(data) {
		return nil, errors.New("restart 数组越界")
	}

	var out []kv
	pos := 0
	var prev []byte
	for pos < end {
		shared, n, err := uvarint(data[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		nonShared, n, err := uvarint(data[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		vlen, n, err := uvarint(data[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		if pos+int(nonShared)+int(vlen) > len(data) {
			return nil, errors.New("条目越界")
		}
		if int(shared) > len(prev) {
			return nil, errors.New("shared 前缀超出上一条 key")
		}
		key := make([]byte, 0, int(shared)+int(nonShared))
		key = append(key, prev[:shared]...)
		key = append(key, data[pos:pos+int(nonShared)]...)
		pos += int(nonShared)
		val := data[pos : pos+int(vlen)]
		pos += int(vlen)
		out = append(out, kv{key, val})
		prev = key
	}
	return out, nil
}

// logRecords 读一个 WAL 文件，返回其中的记录负载。
//
// WAL 按 32KB 分块，每块里是若干物理记录：
//
//	crc32c(4B) | length(2B, LE) | type(1B) | payload
//
// type: 1=FULL 2=FIRST 3=MIDDLE 4=LAST。一条逻辑记录可能跨块，
// 所以要用 FIRST..LAST 把碎片拼起来。这里不校验 crc，只做顺序读取。
func logRecords(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	const blockSize = 32768

	var out [][]byte
	var pending []byte
	pos := 0
	for pos+7 <= len(data) {
		blockLeft := blockSize - (pos % blockSize)
		if blockLeft < 7 { // 块尾填充
			pos += blockLeft
			continue
		}
		length := int(binary.LittleEndian.Uint16(data[pos+4:]))
		rtype := data[pos+6]
		if length == 0 && rtype == 0 { // 预分配的零头，跳到下一块
			pos += blockLeft
			continue
		}
		if pos+7+length > len(data) {
			break // 文件被截断（数据库正在写），丢弃尾巴
		}
		payload := data[pos+7 : pos+7+length]
		pos += 7 + length
		switch rtype {
		case 1: // FULL
			pending = nil
			out = append(out, payload)
		case 2: // FIRST
			pending = append([]byte(nil), payload...)
		case 3: // MIDDLE
			pending = append(pending, payload...)
		case 4: // LAST
			pending = append(pending, payload...)
			out = append(out, pending)
			pending = nil
		}
	}
	return out, nil
}

type batchOp struct {
	del   bool
	key   []byte
	value []byte
}

// decodeWriteBatch 解析一条 WriteBatch。
//
//	sequence(8B) | count(4B) | 若干条记录
//	记录: type(1B) | keyLen(varint) | key | [valueLen(varint) | value]
//	type 1 = PUT，0 = DELETE
func decodeWriteBatch(payload []byte) ([]batchOp, error) {
	if len(payload) < 12 {
		return nil, nil
	}
	var out []batchOp
	pos := 12
	for pos < len(payload) {
		op := payload[pos]
		pos++
		klen, n, err := uvarint(payload[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		if pos+int(klen) > len(payload) {
			return nil, errors.New("batch key 越界")
		}
		key := payload[pos : pos+int(klen)]
		pos += int(klen)
		if op == 1 {
			vlen, n, err := uvarint(payload[pos:])
			if err != nil {
				return nil, err
			}
			pos += n
			if pos+int(vlen) > len(payload) {
				return nil, errors.New("batch value 越界")
			}
			out = append(out, batchOp{key: key, value: payload[pos : pos+int(vlen)]})
			pos += int(vlen)
		} else {
			out = append(out, batchOp{del: true, key: key})
		}
	}
	return out, nil
}

// dbEntries 读一个 LevelDB 目录，返回最新状态的 key/value。
// .ldb 按文件名升序读（编号越大越新，会覆盖旧值），再用 .log 覆盖。
func dbEntries(dir string) ([]kv, error) {
	type slot struct {
		value []byte
	}
	merged := map[string]*slot{}
	var order []string

	put := func(k, v []byte) {
		s := string(k)
		if _, ok := merged[s]; !ok {
			order = append(order, s)
		}
		merged[s] = &slot{value: v}
	}

	ldbs, _ := filepath.Glob(filepath.Join(dir, "*.ldb"))
	sort.Strings(ldbs)
	for _, f := range ldbs {
		ents, err := sstEntries(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, e := range ents {
			put(e.key, e.value)
		}
	}

	logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	sort.Strings(logs)
	for _, f := range logs {
		recs, err := logRecords(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, r := range recs {
			ops, err := decodeWriteBatch(r)
			if err != nil {
				continue // 正在写的尾巴可能不完整，跳过
			}
			for _, op := range ops {
				if op.del {
					delete(merged, string(op.key))
					continue
				}
				put(op.key, op.value)
			}
		}
	}

	out := make([]kv, 0, len(order))
	for _, k := range order {
		if s, ok := merged[k]; ok {
			out = append(out, kv{[]byte(k), s.value})
		}
	}
	return out, nil
}
