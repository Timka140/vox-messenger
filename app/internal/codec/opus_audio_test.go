package codec

import (
	"encoding/binary"
	"math"
	"testing"
)

// Звук демонстрации (48 кГц стерео) кодируется и декодируется кадрами по
// 20 мс, а голосовой кодек при этом остаётся 16 кГц моно.
func TestAudioEncoderStereoRoundTrip(t *testing.T) {
	enc, err := NewAudioEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := NewAudioDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()

	pcm := make([]byte, AudioFrameSize*AudioChannels*2)
	for i := 0; i < AudioFrameSize; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/AudioSampleRate))
		for c := 0; c < AudioChannels; c++ {
			binary.LittleEndian.PutUint16(pcm[(i*AudioChannels+c)*2:], uint16(v))
		}
	}
	var out []byte
	for k := 0; k < 5; k++ {
		pkt, err := enc.Encode(pcm)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if len(pkt) > 2 {
			if out, err = dec.Decode(pkt, false); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
	}
	if len(out) != len(pcm) {
		t.Fatalf("декодировано %d байт, ждали %d", len(out), len(pcm))
	}
	if plc, err := dec.DecodePLC(); err != nil || len(plc) != len(pcm) {
		t.Fatalf("PLC: %d байт, %v", len(plc), err)
	}
	if _, err := enc.Encode(make([]byte, FrameSize*2)); err == nil {
		t.Fatal("кадр голосового размера принят стерео-кодеком")
	}

	voice, err := NewEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer voice.Close()
	if _, err := voice.Encode(make([]byte, FrameSize*2)); err != nil {
		t.Fatalf("голосовой кодек сломался: %v", err)
	}
}
