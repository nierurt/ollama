package llm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/jmorganca/ollama/format"
)

const (
	fileTypeF32 uint32 = iota
	fileTypeF16
	fileTypeQ4_0
	fileTypeQ4_1
	fileTypeQ4_1_F16
	fileTypeQ8_0 uint32 = iota + 2
	fileTypeQ5_0
	fileTypeQ5_1
	fileTypeQ2_K
	fileTypeQ3_K_S
	fileTypeQ3_K_M
	fileTypeQ3_K_L
	fileTypeQ4_K_S
	fileTypeQ4_K_M
	fileTypeQ5_K_S
	fileTypeQ5_K_M
	fileTypeQ6_K
)

func fileType(fileType uint32) string {
	switch fileType {
	case fileTypeF32:
		return "F32"
	case fileTypeF16:
		return "F16"
	case fileTypeQ4_0:
		return "Q4_0"
	case fileTypeQ4_1:
		return "Q4_1"
	case fileTypeQ4_1_F16:
		return "Q4_1_F16"
	case fileTypeQ8_0:
		return "Q8_0"
	case fileTypeQ5_0:
		return "Q5_0"
	case fileTypeQ5_1:
		return "Q5_1"
	case fileTypeQ2_K:
		return "Q2_K"
	case fileTypeQ3_K_S:
		return "Q3_K_S"
	case fileTypeQ3_K_M:
		return "Q3_K_M"
	case fileTypeQ3_K_L:
		return "Q3_K_L"
	case fileTypeQ4_K_S:
		return "Q4_K_S"
	case fileTypeQ4_K_M:
		return "Q4_K_M"
	case fileTypeQ5_K_S:
		return "Q5_K_S"
	case fileTypeQ5_K_M:
		return "Q5_K_M"
	case fileTypeQ6_K:
		return "Q6_K"
	default:
		return "unknown"
	}
}

const (
	// Magic constant for `ggml` files (unversioned).
	FILE_MAGIC_GGML = 0x67676d6c
	// Magic constant for `ggml` files (versioned, ggmf).
	FILE_MAGIC_GGMF = 0x67676d66
	// Magic constant for `ggml` files (versioned, ggjt).
	FILE_MAGIC_GGJT = 0x67676a74
	// Magic constant for `ggla` files (LoRA adapter).
	FILE_MAGIC_GGLA = 0x67676C61
	// Magic constant for `gguf` files (versioned, gguf)
	FILE_MAGIC_GGUF_LE = 0x46554747
	FILE_MAGIC_GGUF_BE = 0x47475546
)

type GGML struct {
	magic uint32
	container
	model
}

type model interface {
	ModelFamily() string
	ModelType() string
	FileType() string
	NumLayers() int64
}

type container interface {
	Name() string
	Decode(io.Reader) (model, error)
}

type containerGGUF struct {
	bo binary.ByteOrder

	Version uint32

	V1 struct {
		NumTensor uint32
		NumKV     uint32
	}

	V2 struct {
		NumTensor uint64
		NumKV     uint64
	}

	parameters uint64
}

const (
	ggufTypeUint8 uint32 = iota
	ggufTypeInt8
	ggufTypeUint16
	ggufTypeInt16
	ggufTypeUint32
	ggufTypeInt32
	ggufTypeFloat32
	ggufTypeBool
	ggufTypeString
	ggufTypeArray
	ggufTypeUint64
	ggufTypeInt64
	ggufTypeFloat64
)

type kv map[string]any

type ggufModel struct {
	*containerGGUF
	kv
}

func newGGUFModel(container *containerGGUF) *ggufModel {
	return &ggufModel{
		containerGGUF: container,
		kv:            make(kv),
	}
}

func (c *containerGGUF) Name() string {
	return "gguf"
}

func (c *containerGGUF) Decode(r io.Reader) (model, error) {
	binary.Read(r, c.bo, &c.Version)

	switch c.Version {
	case 1:
		binary.Read(r, c.bo, &c.V1)
	default:
		binary.Read(r, c.bo, &c.V2)
	}

	model := newGGUFModel(c)
	if err := model.Decode(r); err != nil {
		return nil, err
	}

	return model, nil
}

func (llm *ggufModel) NumTensor() uint64 {
	if llm.Version == 1 {
		return uint64(llm.V1.NumTensor)
	}

	return llm.V2.NumTensor
}

func (llm *ggufModel) NumKV() uint64 {
	if llm.Version == 1 {
		return uint64(llm.V1.NumKV)
	}

	return llm.V2.NumKV
}

func (llm *ggufModel) ModelFamily() string {
	t, ok := llm.kv["general.architecture"].(string)
	if ok {
		return t
	}

	return "unknown"
}

func (llm *ggufModel) ModelType() string {
	if llm.parameters > 0 {
		return format.HumanNumber(llm.parameters)
	}

	switch llm.ModelFamily() {
	case "llama":
		if blocks, ok := llm.kv["llama.block_count"].(uint32); ok {
			heads, headsOK := llm.kv["llama.head_count"].(uint32)
			headKVs, headsKVsOK := llm.kv["llama.head_count_kv"].(uint32)
			if headsOK && headsKVsOK && heads/headKVs == 8 {
				return "70B"
			}

			return llamaModelType(blocks)
		}
	case "falcon":
		if blocks, ok := llm.kv["falcon.block_count"].(uint32); ok {
			return falconModelType(blocks)
		}
	case "starcoder":
		if blocks, ok := llm.kv["starcoder.block_count"].(uint32); ok {
			return starCoderModelType(blocks)
		}
	}

	return "unknown"
}

func (llm *ggufModel) FileType() string {
	t, ok := llm.kv["general.file_type"].(uint32)
	if ok {
		return fileType(t)
	}

	return "unknown"
}

func (llm *ggufModel) Decode(r io.Reader) error {
	// decode key-values
	for i := 0; uint64(i) < llm.NumKV(); i++ {
		k, err := llm.readString(r)
		if err != nil {
			return err
		}

		vtype := llm.readU32(r)

		var v any
		switch vtype {
		case ggufTypeUint8:
			v = llm.readU8(r)
		case ggufTypeInt8:
			v = llm.readI8(r)
		case ggufTypeUint16:
			v = llm.readU16(r)
		case ggufTypeInt16:
			v = llm.readI16(r)
		case ggufTypeUint32:
			v = llm.readU32(r)
		case ggufTypeInt32:
			v = llm.readI32(r)
		case ggufTypeUint64:
			v = llm.readU64(r)
		case ggufTypeInt64:
			v = llm.readI64(r)
		case ggufTypeFloat32:
			v = llm.readF32(r)
		case ggufTypeFloat64:
			v = llm.readF64(r)
		case ggufTypeBool:
			v = llm.readBool(r)
		case ggufTypeString:
			s, err := llm.readString(r)
			if err != nil {
				return err
			}

			v = s
		case ggufTypeArray:
			a, err := llm.readArray(r)
			if err != nil {
				return err
			}

			v = a
		default:
			return fmt.Errorf("invalid type: %d", vtype)
		}

		llm.kv[k] = v
	}

	// decode tensors
	for i := 0; uint64(i) < llm.NumTensor(); i++ {
		if _, err := llm.readString(r); err != nil {
			return err
		}

		dimensions := llm.readU32(r)

		var elements uint64 = 1
		for i := 0; uint32(i) < dimensions; i++ {
			elements *= llm.readU64(r)
		}

		llm.readU32(r) // type
		llm.readU64(r) // offset

		llm.parameters += elements
	}

	return nil
}

func (llm *ggufModel) NumLayers() int64 {
	value, exists := llm.kv[fmt.Sprintf("%s.block_count", llm.ModelFamily())]
	if !exists {
		return 0
	}

	v := value.(uint32)
	return int64(v)
}

func (llm ggufModel) readU8(r io.Reader) uint8 {
	var u8 uint8
	binary.Read(r, llm.bo, &u8)
	return u8
}

func (llm ggufModel) readI8(r io.Reader) int8 {
	var i8 int8
	binary.Read(r, llm.bo, &i8)
	return i8
}

func (llm ggufModel) readU16(r io.Reader) uint16 {
	var u16 uint16
	binary.Read(r, llm.bo, &u16)
	return u16
}

func (llm ggufModel) readI16(r io.Reader) int16 {
	var i16 int16
	binary.Read(r, llm.bo, &i16)
	return i16
}

func (llm ggufModel) readU32(r io.Reader) uint32 {
	var u32 uint32
	binary.Read(r, llm.bo, &u32)
	return u32
}

func (llm ggufModel) readI32(r io.Reader) int32 {
	var i32 int32
	binary.Read(r, llm.bo, &i32)
	return i32
}

func (llm ggufModel) readU64(r io.Reader) uint64 {
	var u64 uint64
	binary.Read(r, llm.bo, &u64)
	return u64
}

func (llm ggufModel) readI64(r io.Reader) int64 {
	var i64 int64
	binary.Read(r, llm.bo, &i64)
	return i64
}

func (llm ggufModel) readF32(r io.Reader) float32 {
	var f32 float32
	binary.Read(r, llm.bo, &f32)
	return f32
}

func (llm ggufModel) readF64(r io.Reader) float64 {
	var f64 float64
	binary.Read(r, llm.bo, &f64)
	return f64
}

func (llm ggufModel) readBool(r io.Reader) bool {
	var b bool
	binary.Read(r, llm.bo, &b)
	return b
}

func (llm ggufModel) readStringV1(r io.Reader) (string, error) {
	var nameLength uint32
	binary.Read(r, llm.bo, &nameLength)

	var b bytes.Buffer
	if _, err := io.CopyN(&b, r, int64(nameLength)); err != nil {
		return "", err
	}

	// gguf v1 strings are null-terminated
	b.Truncate(b.Len() - 1)

	return b.String(), nil
}

func (llm ggufModel) readString(r io.Reader) (string, error) {
	if llm.Version == 1 {
		return llm.readStringV1(r)
	}

	var nameLength uint64
	binary.Read(r, llm.bo, &nameLength)

	var b bytes.Buffer
	if _, err := io.CopyN(&b, r, int64(nameLength)); err != nil {
		return "", err
	}

	return b.String(), nil
}

func (llm *ggufModel) readArrayV1(r io.Reader) (arr []any, err error) {
	atype := llm.readU32(r)
	n := llm.readU32(r)

	for i := 0; uint32(i) < n; i++ {
		switch atype {
		case ggufTypeUint8:
			arr = append(arr, llm.readU8(r))
		case ggufTypeInt8:
			arr = append(arr, llm.readI8(r))
		case ggufTypeUint16:
			arr = append(arr, llm.readU16(r))
		case ggufTypeInt16:
			arr = append(arr, llm.readI16(r))
		case ggufTypeUint32:
			arr = append(arr, llm.readU32(r))
		case ggufTypeInt32:
			arr = append(arr, llm.readI32(r))
		case ggufTypeFloat32:
			arr = append(arr, llm.readF32(r))
		case ggufTypeBool:
			arr = append(arr, llm.readBool(r))
		case ggufTypeString:
			s, err := llm.readStringV1(r)
			if err != nil {
				return nil, err
			}

			arr = append(arr, s)
		default:
			return nil, fmt.Errorf("invalid array type: %d", atype)
		}
	}

	return
}

func (llm *ggufModel) readArray(r io.Reader) (arr []any, err error) {
	if llm.Version == 1 {
		return llm.readArrayV1(r)
	}

	atype := llm.readU32(r)
	n := llm.readU64(r)

	for i := 0; uint64(i) < n; i++ {
		switch atype {
		case ggufTypeUint8:
			arr = append(arr, llm.readU8(r))
		case ggufTypeInt8:
			arr = append(arr, llm.readI8(r))
		case ggufTypeUint16:
			arr = append(arr, llm.readU16(r))
		case ggufTypeInt16:
			arr = append(arr, llm.readI16(r))
		case ggufTypeUint32:
			arr = append(arr, llm.readU32(r))
		case ggufTypeInt32:
			arr = append(arr, llm.readI32(r))
		case ggufTypeUint64:
			arr = append(arr, llm.readU64(r))
		case ggufTypeInt64:
			arr = append(arr, llm.readI64(r))
		case ggufTypeFloat32:
			arr = append(arr, llm.readF32(r))
		case ggufTypeFloat64:
			arr = append(arr, llm.readF64(r))
		case ggufTypeBool:
			arr = append(arr, llm.readBool(r))
		case ggufTypeString:
			s, err := llm.readString(r)
			if err != nil {
				return nil, err
			}

			arr = append(arr, s)
		default:
			return nil, fmt.Errorf("invalid array type: %d", atype)
		}
	}

	return
}

type containerLORA struct {
	version uint32
}

func (c *containerLORA) Name() string {
	return "ggla"
}

func (c *containerLORA) Decode(r io.Reader) (model, error) {
	var version uint32
	binary.Read(r, binary.LittleEndian, &version)

	switch version {
	case 1:
	default:
		return nil, errors.New("invalid version")
	}

	c.version = version
	return nil, nil
}

var ErrUnsupportedFormat = errors.New("unsupported model format")

func DecodeGGML(r io.ReadSeeker) (*GGML, error) {
	var ggml GGML
	binary.Read(r, binary.LittleEndian, &ggml.magic)

	switch ggml.magic {
	case FILE_MAGIC_GGML, FILE_MAGIC_GGMF, FILE_MAGIC_GGJT:
		return nil, ErrUnsupportedFormat
	case FILE_MAGIC_GGLA:
		ggml.container = &containerLORA{}
	case FILE_MAGIC_GGUF_LE:
		ggml.container = &containerGGUF{bo: binary.LittleEndian}
	case FILE_MAGIC_GGUF_BE:
		ggml.container = &containerGGUF{bo: binary.BigEndian}
	default:
		return nil, errors.New("invalid file magic")
	}

	model, err := ggml.Decode(r)
	if err != nil {
		return nil, err
	}

	ggml.model = model

	// final model type
	return &ggml, nil
}
