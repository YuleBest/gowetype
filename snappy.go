package main

import "errors"

var errCorrupt = errors.New("snappy: 数据损坏")

// decodeSnappy 解压 snappy 的 raw block 格式。
//
// LevelDB 的每个 data block 默认用 snappy 压缩（block trailer 的第 1 字节 = 1）。
// 格式很简单，只有三种 element：
//
//	varint 解压后总长度
//	循环:
//	  tag 低 2 位 = 类型
//	    00 literal  —— 后面跟一段原样字节
//	    01 copy     —— 从已解压数据的 offset 处回拷（offset 11 位，长度 4~11）
//	    10 copy     —— offset 16 位，长度 1~64
//	    11 copy     —— offset 32 位，长度 1~64
//
// 自己实现是为了让整个工具零依赖、纯 Go，交叉编译到 Android 不需要 cgo。
func decodeSnappy(src []byte) ([]byte, error) {
	total, pos, err := uvarint(src)
	if err != nil {
		return nil, err
	}
	dst := make([]byte, 0, total)

	for pos < len(src) {
		tag := src[pos]
		pos++
		switch tag & 0x03 {
		case 0x00: // literal
			n := int(tag >> 2)
			if n < 60 {
				n++
			} else {
				extra := n - 59 // 60->1 字节, 61->2, 62->3, 63->4
				if pos+extra > len(src) {
					return nil, errCorrupt
				}
				n = 0
				for i := 0; i < extra; i++ {
					n |= int(src[pos+i]) << (8 * i)
				}
				n++
				pos += extra
			}
			if pos+n > len(src) {
				return nil, errCorrupt
			}
			dst = append(dst, src[pos:pos+n]...)
			pos += n

		case 0x01: // copy, 11 位 offset
			if pos >= len(src) {
				return nil, errCorrupt
			}
			n := 4 + int((tag>>2)&0x07)
			off := int(tag>>5)<<8 | int(src[pos])
			pos++
			var err error
			if dst, err = copyBack(dst, off, n); err != nil {
				return nil, err
			}

		case 0x02: // copy, 16 位 offset
			if pos+2 > len(src) {
				return nil, errCorrupt
			}
			n := 1 + int(tag>>2)
			off := int(src[pos]) | int(src[pos+1])<<8
			pos += 2
			var err error
			if dst, err = copyBack(dst, off, n); err != nil {
				return nil, err
			}

		case 0x03: // copy, 32 位 offset
			if pos+4 > len(src) {
				return nil, errCorrupt
			}
			n := 1 + int(tag>>2)
			off := int(src[pos]) | int(src[pos+1])<<8 |
				int(src[pos+2])<<16 | int(src[pos+3])<<24
			pos += 4
			var err error
			if dst, err = copyBack(dst, off, n); err != nil {
				return nil, err
			}
		}
	}
	if uint64(len(dst)) != total {
		return nil, errCorrupt
	}
	return dst, nil
}

// copyBack 从 dst 末尾回拷 n 字节，源起点是 dst[len-off:]。
// 源和目标可以重叠（LZ77 的 run 就是靠这个展开的），所以必须逐字节。
func copyBack(dst []byte, off, n int) ([]byte, error) {
	if off <= 0 || off > len(dst) {
		return nil, errCorrupt
	}
	start := len(dst) - off
	for i := 0; i < n; i++ {
		dst = append(dst, dst[start+i])
	}
	return dst, nil
}

// uvarint 读 LEB128 变长整数，返回 (值, 消耗的字节数)。
// 注意第二个返回值是消耗长度，不是新的偏移——调用方要自己 pos += n。
func uvarint(buf []byte) (uint64, int, error) {
	var result uint64
	var shift uint
	for i := 0; i < len(buf); i++ {
		b := buf[i]
		if shift > 63 {
			return 0, 0, errCorrupt
		}
		result |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			return result, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, errCorrupt
}
