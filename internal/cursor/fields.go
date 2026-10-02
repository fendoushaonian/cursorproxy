package cursor

import "github.com/michael/cursorproxy/internal/pb"

// pbFields 只收集 length-delimited 字段。响应里的正文、思考和工具调用都是这种。
func pbFields(b []byte) (map[int][][]byte, error) {
	parsed, err := pb.Parse(b)
	if err != nil {
		return nil, err
	}
	out := map[int][][]byte{}
	for _, f := range parsed {
		if f.Wire != pb.WireBytes {
			continue
		}
		out[f.Num] = append(out[f.Num], f.Bytes)
	}
	return out, nil
}
