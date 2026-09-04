// Wire-format encoding for the small subset of Google Flights' internal
// search protobuf schema this package needs (the "tfs" query param).
// Schema (reverse-engineered, matches github.com/AWeirdDev/flights'
// fast_flights/pb/flights.proto):
//
//	message Airport     { string airport = 2; }
//	message FlightData  { string date = 2; Airport from_airport = 13;
//	                       Airport to_airport = 14; optional int32 max_stops = 5;
//	                       repeated string airlines = 6; }
//	message Baggage     { optional int32 carry_on_bags = 2; optional int32 checked_bags = 3; }
//	message Info        { repeated FlightData data = 3; Seat seat = 9;
//	                       repeated Passenger passengers = 8; optional int32 max_price = 12;
//	                       Baggage baggage = 13; Trip trip = 19; }
//
// No protobuf library needed: proto3 wire format is a handful of varint/
// length-delimited primitives, hand-rolled below.
package googleflights

import (
	"bytes"
	"encoding/binary"
)

const (
	wireVarint = 0
	wireBytes  = 2
)

func putTag(buf *bytes.Buffer, field int, wireType int) {
	putVarint(buf, uint64(field<<3|wireType))
}

func putVarint(buf *bytes.Buffer, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	buf.Write(tmp[:n])
}

func putString(buf *bytes.Buffer, field int, s string) {
	putTag(buf, field, wireBytes)
	putVarint(buf, uint64(len(s)))
	buf.WriteString(s)
}

func putBytes(buf *bytes.Buffer, field int, b []byte) {
	putTag(buf, field, wireBytes)
	putVarint(buf, uint64(len(b)))
	buf.Write(b)
}

func putVarintField(buf *bytes.Buffer, field int, v int64) {
	putTag(buf, field, wireVarint)
	putVarint(buf, uint64(v))
}

func putOptionalInt32(buf *bytes.Buffer, field int, v *int) {
	if v == nil {
		return
	}
	putVarintField(buf, field, int64(*v))
}

func putOptionalBool(buf *bytes.Buffer, field int, v *bool) {
	if v == nil {
		return
	}
	if *v {
		putVarintField(buf, field, 1)
	} else {
		putVarintField(buf, field, 0)
	}
}

// putPackedVarints encodes a repeated scalar (proto3 enums/ints default to
// packed encoding): one tag, one length, then every value's varint back to
// back — not one tag per element.
func putPackedVarints(buf *bytes.Buffer, field int, values []int) {
	if len(values) == 0 {
		return
	}
	var inner bytes.Buffer
	for _, v := range values {
		putVarint(&inner, uint64(v))
	}
	putBytes(buf, field, inner.Bytes())
}

// airport encodes message Airport { string airport = 2; }.
func airportMessage(code string) []byte {
	var buf bytes.Buffer
	putString(&buf, 2, code)
	return buf.Bytes()
}

// baggageMessage encodes message Baggage.
func baggageMessage(carryOn, checked *int) []byte {
	var buf bytes.Buffer
	putOptionalInt32(&buf, 2, carryOn)
	putOptionalInt32(&buf, 3, checked)
	return buf.Bytes()
}

// flightDataMessage encodes one leg (message FlightData).
func flightDataMessage(l Leg) []byte {
	var buf bytes.Buffer
	putString(&buf, 2, l.Date)
	putBytes(&buf, 13, airportMessage(l.FromAirport))
	putBytes(&buf, 14, airportMessage(l.ToAirport))
	putOptionalInt32(&buf, 5, l.MaxStops)
	for _, a := range l.Airlines {
		putString(&buf, 6, a)
	}
	return buf.Bytes()
}

// infoMessage encodes the top-level Info message that becomes the base64
// "tfs" query param.
func infoMessage(q Query) []byte {
	var buf bytes.Buffer
	for _, leg := range q.Legs {
		putBytes(&buf, 3, flightDataMessage(leg))
	}
	putVarintField(&buf, 9, int64(q.Seat))
	putPackedVarints(&buf, 8, passengersToEnum(q.Passengers))
	putOptionalInt32(&buf, 12, q.MaxPrice)
	if q.CarryOnBags != nil || q.CheckedBags != nil {
		putBytes(&buf, 13, baggageMessage(q.CarryOnBags, q.CheckedBags))
	}
	putVarintField(&buf, 19, int64(q.Trip))
	return buf.Bytes()
}
