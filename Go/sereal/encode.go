package sereal

import (
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"unsafe"

	"code.google.com/p/snappy-go/snappy"
)

func reflectValueOf(v interface{}) reflect.Value {

	rv, ok := v.(reflect.Value)
	if !ok {
		rv = reflect.ValueOf(v)
	}
	return rv

}

func snappify(b []byte) ([]byte, error) {
	b[4] |= (2 << 4) // set the document type to '2' (incr snappy)

	optHeaderLength, optHeaderSize := varintdecode(b[5:])
	optHeaderLength += optHeaderSize

	// XXX this could be more efficient!  I'm creating a new buffer to
	//     store the compressed document, which isn't necessary.  You
	//     could probably write directly to the slice after the header
	//     and after the varint holding the length
	compressed, err := snappy.Encode(nil, b[5+optHeaderLength:])
	if err != nil {
		return nil, err
	}

	// resize b to just the original header
	b = b[:5+optHeaderLength]
	b = varint(b, uint(len(compressed)))
	b = append(b, compressed...)

	return b, nil
}

// An Encoder encodes Go data structures into Sereal byte streams
type Encoder struct {
	PerlCompat      bool // try to mimic Perl's structure as much as possible
	UseSnappy       bool // should we enable snappy compression
	SnappyThreshold int  // threshold in bytes above which snappy compression is attempted: 1024 bytes by default
	DisableDedup    bool // should we disable deduping of class names and hash keys
	DisableFREEZE   bool // should we disable the FREEZE tag, which calls MarshalBinary
	version         int  // default version to encode
}

// NewEncoder returns a new Encoder struct with default values
func NewEncoder() *Encoder {
	return &Encoder{
		PerlCompat:      false,
		UseSnappy:       false,
		SnappyThreshold: 1024,
		version:         1,
	}
}

// NewEncoderV2 returns a new Encoder that encodes version 2
func NewEncoderV2() *Encoder {
	return &Encoder{
		PerlCompat:      false,
		UseSnappy:       false,
		SnappyThreshold: 1024,
		version:         2,
	}
}

var defaultEncoder = NewEncoderV2()

// Marshal encodes body with the default encoder
func Marshal(body interface{}) ([]byte, error) {
	return defaultEncoder.MarshalWithHeader(nil, body)
}

// Marshal returns the Sereal encoding of body
func (e *Encoder) Marshal(body interface{}) (b []byte, err error) {
	return e.MarshalWithHeader(nil, body)
}

// MarshalWithHeader returns the Sereal encoding of body with header data
func (e *Encoder) MarshalWithHeader(header interface{}, body interface{}) (b []byte, err error) {
	defer func() {
		//return
		if r := recover(); r != nil {
			if _, ok := r.(runtime.Error); ok {
				panic(r)
			}

			if s, ok := r.(string); ok {
				err = errors.New(s)
			} else {
				err = r.(error)
			}
		}
	}()

	// TODO(mvuets) Better set to the latest version, since defaultEncoder is the latest already.
	// uninitialized encoder? set to version 1
	if e.version == 0 {
		e.version = 1
	}

	b = make([]byte, headerSize, 32)

	binary.LittleEndian.PutUint32(b[:4], magicHeaderBytes)

	b[4] = byte(e.version)

	if header != nil && e.version >= 2 {
		strTable := make(map[string]int)
		ptrTable := make(map[uintptr]int)
		rv := reflectValueOf(header)
		// this is both the flag byte (== "there is user data") and also a hack to make 1-based offsets work
		henv := []byte{0x01} // flag byte == "there is user data"
		encoded, err := e.encode(henv, rv, false, strTable, ptrTable)

		if err != nil {
			return nil, err
		}

		b = varint(b, uint(len(encoded)))
		b = append(b, encoded...)
	} else {
		/* header size */
		b = append(b, 0)
	}

	rv := reflectValueOf(body)

	strTable := make(map[string]int)
	ptrTable := make(map[uintptr]int)

	var encoded []byte

	switch e.version {
	case 1:
		encoded, err = e.encode(b, rv, false, strTable, ptrTable)
	case 2:
		benc := []byte{0} // hack for 1-based offsets
		encoded, err = e.encode(benc, rv, false, strTable, ptrTable)
		encoded = encoded[1:] // trim hacky first byte
		encoded = append(b, encoded...)
	}

	if err != nil {
		return nil, err
	}

	if e.UseSnappy && (e.SnappyThreshold == 0 || len(encoded) >= e.SnappyThreshold) {
		encoded, err = snappify(encoded)

		if err != nil {
			return nil, err
		}
	}

	return encoded, nil
}

func (e *Encoder) encode(b []byte, rv reflect.Value, isKeyOrClass bool, strTable map[string]int, ptrTable map[uintptr]int) ([]byte, error) {

	if !e.DisableFREEZE && rv.Kind() != reflect.Invalid {
		if m, ok := rv.Interface().(encoding.BinaryMarshaler); ok {
			by, err := m.MarshalBinary()
			if err != nil {
				return nil, err
			}

			name := concreteName(rv)

			b = append(b, typeOBJECT_FREEZE)
			b, err = e.encode(b, reflect.ValueOf(name), true, strTable, ptrTable)
			if err != nil {
				return nil, err
			}

			b, err = e.encode(b, reflect.ValueOf(by), false, strTable, ptrTable)
			if err != nil {
				return nil, err
			}
			return b, nil
		}
	}

	// make sure we're looking at a real type and not an interface
	for rv.Kind() == reflect.Interface {
		rv = rv.Elem()
	}

	switch rk := rv.Kind(); rk {

	case reflect.Bool:
		b = e.encodeBool(b, rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int64, reflect.Int32, reflect.Int16:
		b = e.encodeInt(b, reflect.Int, rv.Int())
	case reflect.Uint8, reflect.Uint64, reflect.Uint, reflect.Uint32, reflect.Uint16:
		b = e.encodeInt(b, reflect.Uint, int64(rv.Uint()))
	case reflect.String:
		b = e.encodeString(b, rv.String(), isKeyOrClass, strTable)
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			b = e.encodeBytes(b, rv.Bytes(), isKeyOrClass, strTable)
			break
		}
		fallthrough

	case reflect.Array:
		b = e.encodeArray(b, rv, strTable, ptrTable)

	case reflect.Map:
		b = e.encodeMap(b, rv, strTable, ptrTable)

	case reflect.Struct:
		b = e.encodeStruct(b, rv, strTable, ptrTable)

	case reflect.Float32:
		b = e.encodeFloat(b, float32(rv.Float()))

	case reflect.Float64:
		b = e.encodeDouble(b, float64(rv.Float()))

	case reflect.Interface:
		// recurse until we get a concrete type
		// could be optmized into a tail call
		var err error
		b, err = e.encode(b, rv.Elem(), isKeyOrClass, strTable, ptrTable)
		if err != nil {
			return nil, err
		}

	case reflect.Ptr:

		if rv.Elem().Kind() == reflect.Struct {

			switch rv.Elem().Interface().(type) {
			case PerlRegexp:
				b = e.encodeStruct(b, rv.Elem(), strTable, ptrTable)
				return b, nil
			case PerlUndef:
				b = e.encodeStruct(b, rv.Elem(), strTable, ptrTable)
				return b, nil
			case PerlObject:
				b = e.encodeStruct(b, rv.Elem(), strTable, ptrTable)
				return b, nil
			case PerlWeakRef:
				b = e.encodeStruct(b, rv.Elem(), strTable, ptrTable)
				return b, nil
			}
		}

		rvptr := rv.Pointer()
		rvptr2 := getPointer(rv.Elem())

		offs, ok := ptrTable[rvptr]

		if !ok && rvptr2 != 0 {
			offs, ok = ptrTable[rvptr2]
			if ok {
				rvptr = rvptr2
			}
		}

		if ok { // seen this before
			b = append(b, typeREFP)
			b = varint(b, uint(offs))
			b[offs] |= trackFlag // original offset now tracked
		} else {

			lenbOrig := len(b)

			b = append(b, typeREFN)

			ptrTable[rvptr] = lenbOrig

			var err error

			b, err = e.encode(b, rv.Elem(), isKeyOrClass, strTable, ptrTable)
			if err != nil {
				return nil, err
			}

			if rvptr2 != 0 {
				// The thing this this points to starts one after the current pointer
				ptrTable[rvptr2] = lenbOrig + 1
			}
		}

	case reflect.Invalid:
		b = append(b, typeUNDEF)

	default:
		panic(fmt.Sprintf("no support for type '%s'", rk.String()))
	}

	return b, nil
}

func getPointer(rv reflect.Value) uintptr {

	var rvptr uintptr

	switch rv.Kind() {
	case reflect.Map, reflect.Slice:
		rvptr = rv.Pointer()
	case reflect.Interface:
		// FIXME: still needed?
		return getPointer(rv.Elem())
	case reflect.Ptr:
		rvptr = rv.Pointer()
	case reflect.String:
		ps := (*reflect.StringHeader)(unsafe.Pointer(rv.UnsafeAddr()))
		rvptr = ps.Data
	}

	return rvptr
}

func varint(by []byte, n uint) []uint8 {

	for n >= 0x80 {
		b := byte(n) | 0x80
		by = append(by, b)
		n >>= 7
	}

	return append(by, byte(n))
}

func (e *Encoder) encodeArray(by []byte, arr reflect.Value, strTable map[string]int, ptrTable map[uintptr]int) []byte {

	l := arr.Len()

	by = append(by, typeARRAY)
	by = varint(by, uint(l))

	if e.PerlCompat {
		for i := 0; i < l; i++ {
			v := arr.Index(i)
			by = e.encodeScalar(by, v, false, strTable, ptrTable)
		}
	} else {
		for i := 0; i < l; i++ {
			v := arr.Index(i)
			by, _ = e.encode(by, v, false, strTable, ptrTable)
		}
	}

	return by

}

func (e *Encoder) encodeBool(by []byte, bo bool) []byte {

	if bo {
		by = append(by, typeTRUE)
	} else {

		by = append(by, typeFALSE)
	}

	return by
}

func (e *Encoder) encodeBytes(by []byte, byt []byte, isKeyOrClass bool, strTable map[string]int) []byte {

	l := len(byt)

	if l < 32 {

		if !e.DisableDedup && isKeyOrClass {

			// track short byte strTable

			s := string(byt)

			copyOffs, ok := strTable[s]

			if ok {
				by = append(by, typeCOPY)
				by = varint(by, uint(copyOffs))
				return by
			}

			// save for later
			strTable[s] = len(by)
		}

		by = append(by, typeSHORT_BINARY_0+byte(l))
	} else {
		by = append(by, typeBINARY)
		by = varint(by, uint(l))
	}

	by = append(by, byt...)

	return by
}

func (e *Encoder) encodeDouble(by []byte, f float64) []byte {

	var u uint64
	u = math.Float64bits(f)
	by = append(by, typeDOUBLE)
	by = append(by, byte(u))
	by = append(by, byte(u>>8))
	by = append(by, byte(u>>16))
	by = append(by, byte(u>>24))
	by = append(by, byte(u>>32))
	by = append(by, byte(u>>40))
	by = append(by, byte(u>>48))
	by = append(by, byte(u>>56))
	return by
}

func (e *Encoder) encodeFloat(by []byte, f float32) []byte {

	var u uint32
	u = math.Float32bits(f)
	by = append(by, typeFLOAT)
	by = append(by, byte(u))
	by = append(by, byte(u>>8))
	by = append(by, byte(u>>16))
	by = append(by, byte(u>>24))
	return by
}

func (e *Encoder) encodeInt(by []byte, k reflect.Kind, i int64) []byte {

	switch {
	case 0 <= i && i <= 15:
		by = append(by, byte(i)&0x0f)
	case -16 <= i && i < 0 && k == reflect.Int:
		by = append(by, 0x010|(byte(i)&0x0f))
	case i > 15:
		by = append(by, typeVARINT)
		by = varint(by, uint(i))
	case i < 0:
		n := uint(i)
		if k == reflect.Int {
			by = append(by, typeZIGZAG)
			n = uint((i << 1) ^ (i >> 63))
		} else {
			by = append(by, typeVARINT)
		}
		by = varint(by, uint(n))
	}

	return by
}

func (e *Encoder) encodeScalar(by []byte, rv reflect.Value, isKeyOrClass bool, strTable map[string]int, ptrTable map[uintptr]int) []byte {

	for rv.Kind() == reflect.Interface {
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Array, reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			by = e.encodeBytes(by, rv.Bytes(), isKeyOrClass, strTable)
		} else {
			by = append(by, typeREFN)
			by = e.encodeArray(by, rv, strTable, ptrTable)
		}
	case reflect.Map:
		by = append(by, typeREFN)
		by = e.encodeMap(by, rv, strTable, ptrTable)
	case reflect.String:
		by = e.encodeString(by, rv.String(), true, strTable)
	default:
		by, _ = e.encode(by, rv, isKeyOrClass, strTable, ptrTable)
	}

	return by

}

func (e *Encoder) encodeMap(by []byte, m reflect.Value, strTable map[string]int, ptrTable map[uintptr]int) []byte {

	keys := m.MapKeys()

	l := len(keys)

	by = append(by, typeHASH)
	by = varint(by, uint(l))

	if e.PerlCompat {
		for _, k := range keys {
			by = e.encodeString(by, k.String(), true, strTable)
			v := m.MapIndex(k)
			by = e.encodeScalar(by, v, false, strTable, ptrTable)
		}
	} else {
		for _, k := range keys {
			by, _ = e.encode(by, k, true, strTable, ptrTable)
			v := m.MapIndex(k)
			by, _ = e.encode(by, v, false, strTable, ptrTable)
		}
	}

	return by
}

func (e *Encoder) encodeString(by []byte, s string, isKeyOrClass bool, strTable map[string]int) []byte {

	if !e.DisableDedup && isKeyOrClass {

		copyOffs, ok := strTable[s]

		if ok {
			by = append(by, typeCOPY)
			by = varint(by, uint(copyOffs))
			return by
		}

		// save for later
		strTable[s] = len(by)

	}

	by = append(by, typeSTR_UTF8)
	by = varint(by, uint(len(s)))
	by = append(by, s...)

	return by
}

func (e *Encoder) encodeStruct(by []byte, st reflect.Value, strTable map[string]int, ptrTable map[uintptr]int) []byte {

	typ := st.Type()

	// first test if it's one of our internal SerealPerl structs
	switch val := st.Interface().(type) {
	case PerlUndef:
		_ = val
		by = append(by, typeUNDEF)
		return by
	case PerlObject:
		by = append(by, typeOBJECT)
		by = e.encodeBytes(by, []byte(val.Class), true, strTable)
		by, _ = e.encode(by, reflect.ValueOf(val.Reference), false, strTable, ptrTable)
		return by
	case PerlRegexp:
		by = append(by, typeREGEXP)
		by = e.encodeBytes(by, []byte(val.Pattern), false, strTable)
		by = e.encodeBytes(by, []byte(val.Modifiers), false, strTable)
		return by
	case PerlWeakRef:
		by = append(by, typeWEAKEN)
		by, _ = e.encode(by, reflect.ValueOf(val.Reference), false, strTable, ptrTable)
		return by
	}

	by = append(by, typeOBJECT)

	by = e.encodeBytes(by, []byte(typ.Name()), true, strTable)

	tags := getStructTags(st)
	publicFields := len(tags)

	if e.PerlCompat {
		// must be a reference
		by = append(by, typeREFN)
	}

	by = append(by, typeHASH)
	by = varint(by, uint(publicFields))

	if tags != nil {
		for f, i := range tags {
			by = e.encodeString(by, f, true, strTable)
			by, _ = e.encode(by, st.Field(i), false, strTable, ptrTable)
		}
	}

	return by
}

func concreteName(value reflect.Value) string {
	name := value.Type().PkgPath() + "." + value.Type().Name()
	return name
}
