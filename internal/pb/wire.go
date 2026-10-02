// Package pb 是只够 Cursor Connect-RPC 用的 protobuf 线格式读写。
// 不引入 protoc：字段号会随客户端版本变，手写比生成代码更容易对上抓包。
package pb

import "fmt"

const (
	WireVarint = 0
	Wire64     = 1
	WireBytes  = 2
	Wire32     = 5
)

// Field 是一条已切开的 protobuf 字段。Bytes 只在 WireBytes 时有意义。
type Field struct {
	Num    int
	Wire   int
	Varint uint64
	Bytes  []byte
}

// AppendVarint 追加无符号 varint。
func AppendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// AppendBytes 追加 length-delimited 字段。空切片也会写出 tag 和长度 0，
// Cursor 的若干占位字段必须在线上出现。
func AppendBytes(dst []byte, field int, b []byte) []byte {
	dst = AppendVarint(dst, uint64(field<<3|WireBytes))
	dst = AppendVarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// AppendString 追加 UTF-8 字符串字段。
func AppendString(dst []byte, field int, s string) []byte {
	return AppendBytes(dst, field, []byte(s))
}

// AppendVarintField 追加 varint 字段。
func AppendVarintField(dst []byte, field int, v uint64) []byte {
	dst = AppendVarint(dst, uint64(field<<3|WireVarint))
	return AppendVarint(dst, v)
}

// Parse 把一条完整 message 切成字段。遇到截断或未知 wire type 返回错误，
// 避免把半包当成正常响应吞掉。
func Parse(b []byte) ([]Field, error) {
	var out []Field
	i := 0
	for i < len(b) {
		tag, n, err := readVarint(b[i:])
		if err != nil {
			return nil, fmt.Errorf("tag: %w", err)
		}
		i += n
		num := int(tag >> 3)
		wire := int(tag & 7)
		f := Field{Num: num, Wire: wire}
		switch wire {
		case WireVarint:
			v, n, err := readVarint(b[i:])
			if err != nil {
				return nil, fmt.Errorf("field %d: %w", num, err)
			}
			f.Varint = v
			i += n
		case WireBytes:
			l, n, err := readVarint(b[i:])
			if err != nil {
				return nil, fmt.Errorf("field %d len: %w", num, err)
			}
			i += n
			if uint64(len(b)-i) < l {
				return nil, fmt.Errorf("field %d: truncated %d/%d", num, len(b)-i, l)
			}
			f.Bytes = b[i : i+int(l)]
			i += int(l)
		case Wire32:
			if len(b)-i < 4 {
				return nil, fmt.Errorf("field %d: truncated fixed32", num)
			}
			f.Bytes = b[i : i+4]
			i += 4
		case Wire64:
			if len(b)-i < 8 {
				return nil, fmt.Errorf("field %d: truncated fixed64", num)
			}
			f.Bytes = b[i : i+8]
			i += 8
		default:
			return nil, fmt.Errorf("field %d: wire type %d", num, wire)
		}
		out = append(out, f)
	}
	return out, nil
}

func readVarint(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * uint(i))
		if b[i] < 0x80 {
			return v, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("truncated varint")
}
