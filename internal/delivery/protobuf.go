package delivery

import (
	"errors"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	MaxBatchRecords  = 2000
	MaxBatchElements = 128 << 10
	MaxBatchDepth    = 32
)

var (
	ErrBatchRecords  = errors.New("delivery protobuf exceeds 2000 records")
	ErrBatchElements = errors.New("delivery protobuf exceeds 131072 repeated elements")
	ErrBatchDepth    = errors.New("delivery protobuf exceeds nesting depth 32")
	ErrBatchWire     = errors.New("invalid delivery protobuf wire format")
)

type batchWireBudget struct {
	records  int
	elements int
}

// ValidateBatchRecords checks a restored batch before Unmarshal can allocate
// an object for every tiny repeated message/string. Known messages are visited
// using their schema; arbitrary bytes and unknown fields remain opaque. Limits
// bound record/element amplification, not the decoder's complete heap usage.
func ValidateBatchRecords(data []byte, descriptor protoreflect.MessageDescriptor, recordFields ...protowire.Number) error {
	var budget batchWireBudget
	return validateBatchMessage(data, descriptor, recordFields, 1, &budget)
}

// MessageElements counts the same known repeated/map/packed elements before
// serialization. depth includes the surrounding batch and message envelopes.
// Unknown wire is checked too, without marshaling the object on admission.
func MessageElements(message protoreflect.Message, depth int) (int, error) {
	var budget batchWireBudget
	err := countMessageElements(message, depth, &budget)
	return budget.elements, err
}

func countMessageElements(message protoreflect.Message, depth int, budget *batchWireBudget) error {
	if depth > MaxBatchDepth {
		return ErrBatchDepth
	}
	var err error
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsMap():
			entries := value.Map()
			err = budget.addElements(entries.Len())
			if err == nil && entries.Len() > 0 && depth+1 > MaxBatchDepth {
				err = ErrBatchDepth
			}
			if err == nil && field.MapValue().Kind() == protoreflect.MessageKind {
				entries.Range(func(_ protoreflect.MapKey, value protoreflect.Value) bool {
					// A map entry and its message value each add an envelope.
					err = countMessageElements(value.Message(), depth+2, budget)
					return err == nil
				})
			}
		case field.IsList():
			items := value.List()
			err = budget.addElements(items.Len())
			if err == nil && (field.Kind() == protoreflect.MessageKind || field.Kind() == protoreflect.GroupKind) {
				for i := 0; i < items.Len(); i++ {
					if err = countMessageElements(items.Get(i).Message(), depth+1, budget); err != nil {
						break
					}
				}
			}
		case field.Kind() == protoreflect.MessageKind || field.Kind() == protoreflect.GroupKind:
			err = countMessageElements(value.Message(), depth+1, budget)
		}
		return err == nil
	})
	if err != nil {
		return err
	}
	return validateBatchMessage(message.GetUnknown(), message.Descriptor(), nil, depth, budget)
}

func validateBatchMessage(data []byte, descriptor protoreflect.MessageDescriptor, recordFields []protowire.Number, depth int, budget *batchWireBudget) error {
	if depth > MaxBatchDepth {
		return ErrBatchDepth
	}
	fields := descriptor.Fields()
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return ErrBatchWire
		}
		data = data[n:]
		payload, n, err := consumeBatchValue(number, wireType, data, MaxBatchDepth-depth)
		if err != nil {
			return err
		}
		data = data[n:]
		if depth == 1 {
			for _, recordField := range recordFields {
				if number == recordField {
					if wireType != protowire.BytesType {
						return ErrBatchWire
					}
					budget.records++
					if budget.records > MaxBatchRecords {
						return ErrBatchRecords
					}
					break
				}
			}
		}
		field := fields.ByNumber(number)
		if field == nil {
			continue
		}
		if field.IsList() || field.IsMap() {
			if wireType == protowire.BytesType && field.Kind() != protoreflect.MessageKind && field.Kind() != protoreflect.StringKind && field.Kind() != protoreflect.BytesKind {
				// Packed numeric fields can encode many elements with one tag.
				if err := budget.addPacked(field.Kind(), payload); err != nil {
					return err
				}
			} else if err := budget.addElements(1); err != nil {
				return err
			}
		}
		if wireType == protowire.BytesType && field.Kind() == protoreflect.MessageKind || wireType == protowire.StartGroupType && field.Kind() == protoreflect.GroupKind {
			if err := validateBatchMessage(payload, field.Message(), nil, depth+1, budget); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *batchWireBudget) addElements(count int) error {
	if count > MaxBatchElements-b.elements {
		return ErrBatchElements
	}
	b.elements += count
	return nil
}

func (b *batchWireBudget) addPacked(kind protoreflect.Kind, data []byte) error {
	var width int
	switch kind {
	case protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind, protoreflect.FloatKind:
		width = 4
	case protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind, protoreflect.DoubleKind:
		width = 8
	}
	if width > 0 {
		if len(data)%width != 0 {
			return ErrBatchWire
		}
		return b.addElements(len(data) / width)
	}
	for len(data) > 0 {
		_, n := protowire.ConsumeVarint(data)
		if n < 0 {
			return ErrBatchWire
		}
		if err := b.addElements(1); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func consumeBatchValue(number protowire.Number, wireType protowire.Type, data []byte, groupLimit int) ([]byte, int, error) {
	if wireType == protowire.BytesType {
		payload, n := protowire.ConsumeBytes(data)
		if n < 0 {
			return nil, 0, ErrBatchWire
		}
		return payload, n, nil
	}
	if wireType == protowire.StartGroupType {
		return consumeBatchGroup(number, data, groupLimit)
	}
	// ConsumeFieldValue's default group recursion limit is much larger than our
	// budget. Groups are handled separately with an iterative bounded stack.
	n := protowire.ConsumeFieldValue(number, wireType, data)
	if n < 0 {
		return nil, 0, ErrBatchWire
	}
	return nil, n, nil
}

func consumeBatchGroup(number protowire.Number, data []byte, limit int) ([]byte, int, error) {
	if limit <= 0 {
		return nil, 0, ErrBatchDepth
	}
	var stack [MaxBatchDepth]protowire.Number
	stack[0] = number
	depth := 1
	rest := data
	for len(rest) > 0 {
		number, wireType, n := protowire.ConsumeTag(rest)
		if n < 0 {
			return nil, 0, ErrBatchWire
		}
		rest = rest[n:]
		switch wireType {
		case protowire.StartGroupType:
			if depth >= limit {
				return nil, 0, ErrBatchDepth
			}
			stack[depth] = number
			depth++
		case protowire.EndGroupType:
			if stack[depth-1] != number {
				return nil, 0, ErrBatchWire
			}
			depth--
			if depth == 0 {
				consumed := len(data) - len(rest)
				return data[:consumed-n], consumed, nil
			}
		default:
			n := protowire.ConsumeFieldValue(number, wireType, rest)
			if n < 0 {
				return nil, 0, ErrBatchWire
			}
			rest = rest[n:]
		}
	}
	return nil, 0, ErrBatchWire
}
