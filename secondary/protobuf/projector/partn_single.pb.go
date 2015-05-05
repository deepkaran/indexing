// Code generated by protoc-gen-go.
// source: partn_single.proto
// DO NOT EDIT!

package protobuf

import proto "github.com/golang/protobuf/proto"
import math "math"

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = math.Inf

// SinglePartition is an oxymoron - the purpose of partition is to
// scale-out, but using this partition-scheme for an index means the full
// data set is kept on the same node.
type SinglePartition struct {
	Endpoints        []string `protobuf:"bytes,1,rep,name=endpoints" json:"endpoints,omitempty"`
	CoordEndpoint    *string  `protobuf:"bytes,2,opt,name=coordEndpoint" json:"coordEndpoint,omitempty"`
	XXX_unrecognized []byte   `json:"-"`
}

func (m *SinglePartition) Reset()         { *m = SinglePartition{} }
func (m *SinglePartition) String() string { return proto.CompactTextString(m) }
func (*SinglePartition) ProtoMessage()    {}

func (m *SinglePartition) GetEndpoints() []string {
	if m != nil {
		return m.Endpoints
	}
	return nil
}

func (m *SinglePartition) GetCoordEndpoint() string {
	if m != nil && m.CoordEndpoint != nil {
		return *m.CoordEndpoint
	}
	return ""
}

func init() {
}
