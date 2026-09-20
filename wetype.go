package main

import (
	"bytes"
	"encoding/binary"
	"sort"
	"strings"
	"unicode/utf8"
)

// 这个文件解码微信输入法自己的 userdict 结构。
//
// 数据库里所有 key 都带一个前缀，前缀决定这条记录是什么：
//
//	!u_d_v_p!<拼音>\x01<词>\x01<id>              用户词典条目，分隔干净，最可靠
//	!v2u!<拼音>\x01<suffix>                      value 尾部是 u32 词频 + UTF-8 词
//	!user_bi gram2!<flag>\x7f<拼音>\x7f<词>\x01<id>  二元组（上文拼音 + 本词）
//	!user_pr efix2!...                           前缀记录
//	!dcact!<8字节序号>                            value 是 03 + 长度 + "拼音\x01词"
//	!en_fe!c / !en_fe!s                          英文词
//	!u_e_s!\x00                                  表情/符号
//
// 中文词条主要来自前四个前缀。其余几个是引擎内部的频次/上下文统计，
// 对"导出个人词库"没用，直接忽略。

type wordEntry struct {
	Word   string
	Pinyin string
	Score  uint32
	Source string
}

// userDictPrefixes 是词库记录的 key 前缀。探测阶段用它判断一个 LevelDB
// 到底是不是微信输入法的词库（不是所有 LevelDB 都是）。
var userDictPrefixes = []string{"!u_d_v_p!", "!v2u!", "!user_bi", "!dcact!"}

func isCJK(r rune) bool {
	return (r >= 0x3400 && r <= 0x4DBF) || // 扩展 A
		(r >= 0x4E00 && r <= 0x9FFF) || // 基本区
		(r >= 0xF900 && r <= 0xFAFF) // 兼容汉字
}

func hasCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

func cjkCount(s string) int {
	n := 0
	for _, r := range s {
		if isCJK(r) {
			n++
		}
	}
	return n
}

// lastSyllables 从 "bai,fen,ba" 里取末尾 n 个音节，得到 "ba"。
//
// !v2u! 的 key 里存的是「上文 + 本词」的完整拼音，本词拼音是它的后缀；
// 音节数等于词里的汉字数，所以按字数切末尾即可。
func lastSyllables(pinyin string, n int) string {
	if n <= 0 {
		return pinyin
	}
	parts := strings.Split(pinyin, ",")
	if len(parts) < n {
		return pinyin
	}
	return strings.Join(parts[len(parts)-n:], ",")
}

// decodeStrField 解析 value 开头那个 `03 + LE16 长度 + UTF-8` 字段。
func decodeStrField(v []byte) (string, bool) {
	if len(v) < 3 || v[0] != 0x03 {
		return "", false
	}
	n := int(binary.LittleEndian.Uint16(v[1:3]))
	if 3+n > len(v) {
		return "", false
	}
	s := v[3 : 3+n]
	if !utf8.Valid(s) {
		return "", false
	}
	return string(s), true
}

// trailingWord 从 !v2u! 的 value 里切出末尾的词和词频。
//
// value 结构是「变长二进制头部 + u32 词频 + UTF-8 词（一直到结尾）」，
// 头部长度不固定（不同记录差十几字节），没法直接算偏移。
// 所以从结尾往前找起点 i：要求 v[i:] 是严格合法的 UTF-8、不含控制字符、
// 且含有汉字。二进制头部几乎不可能同时满足这三条，实测很稳。
func trailingWord(v []byte) (string, uint32) {
	limit := len(v) - 80
	if limit < 0 {
		limit = 0
	}
	for i := limit; i < len(v)-2; i++ {
		s := v[i:]
		if !utf8.Valid(s) {
			continue
		}
		ctrl := false
		for _, b := range s {
			if b < 0x20 {
				ctrl = true
				break
			}
		}
		if ctrl {
			continue
		}
		word := string(s)
		if !hasCJK(word) {
			continue
		}
		var score uint32
		if i >= 4 {
			score = binary.LittleEndian.Uint32(v[i-4 : i])
		}
		return word, score
	}
	return "", 0
}

// parseUserDict 遍历整个数据库，返回去重后的词表。
func parseUserDict(entries []kv, stats map[string]int) map[string]*wordEntry {
	words := map[string]*wordEntry{}

	add := func(word, pinyin string, score uint32, source string, preferPinyin bool) {
		if word == "" || !hasCJK(word) {
			return
		}
		cur, ok := words[word]
		if !ok {
			words[word] = &wordEntry{word, pinyin, score, source}
			return
		}
		// 拼音以 !u_d_v_p! 为准：那是词本身的拼音，不带上文
		if preferPinyin && cur.Source != "u_d_v_p" {
			cur.Pinyin = pinyin
			cur.Source = source
		}
		if score > cur.Score {
			cur.Score = score
		}
	}

	for _, e := range entries {
		k, v := e.key, e.value

		switch {
		case bytes.HasPrefix(k, []byte("!u_d_v_p!")):
			// !u_d_v_p!拼音\x01词\x01id
			parts := bytes.Split(k[9:], []byte{0x01})
			if len(parts) >= 2 {
				stats["u_d_v_p"]++
				add(string(parts[1]), string(parts[0]), 0, "u_d_v_p", true)
			}

		case bytes.HasPrefix(k, []byte("!v2u!")):
			// key 里 \x01 之前是拼音
			body := k[5:]
			py := body
			if i := bytes.IndexByte(body, 0x01); i >= 0 {
				py = body[:i]
			}
			word, score := trailingWord(v)
			if word != "" {
				stats["v2u"]++
				add(word, lastSyllables(string(py), cjkCount(word)), score, "v2u", false)
			}

		case bytes.HasPrefix(k, []byte("!user_bi")):
			// gram2!<flag>\x7f拼音\x7f词\x01id
			body := k[8:]
			if len(body) > 0 && body[0] == 0x7f {
				head := body
				if i := bytes.IndexByte(body, 0x01); i >= 0 {
					head = body[:i]
				}
				segs := bytes.Split(head, []byte{0x7f})
				if len(segs) >= 3 {
					py := segs[len(segs)-2]
					word := string(segs[len(segs)-1])
					stats["user_bi"]++
					add(word, lastSyllables(string(py), cjkCount(word)), 0, "user_bi", false)
				}
			}

		case bytes.HasPrefix(k, []byte("!dcact!")):
			if s, ok := decodeStrField(v); ok {
				if i := strings.IndexByte(s, 0x01); i >= 0 {
					stats["dcact"]++
					add(s[i+1:], s[:i], 0, "dcact", true)
				}
			}
		}
	}
	return words
}

func sortEntries(words map[string]*wordEntry) []*wordEntry {
	out := make([]*wordEntry, 0, len(words))
	for _, w := range words {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Word < out[j].Word
	})
	return out
}
