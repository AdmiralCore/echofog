//go:build windows

package nativecap

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Media Foundation's hardware H.264 encoders (NVIDIA, AMD, Intel) are asynchronous MFTs: they say
// when they want a frame and when one is ready, through events. Frames go in as D3D11 textures
// of the shared device, so nothing is copied to the CPU before the bits come out.
var (
	iidIMFTransform           = guid("bf94c121-5b05-4e6f-8000-ba598961414d")
	iidIMFMediaEventGenerator = guid("2cd0bd52-bcd5-4b89-b62c-eadc0c031e7d")
	iidICodecAPI              = guid("901db4c7-31ce-41a2-85dc-8fa0bf41b8da")
	iidIMF2DBuffer            = guid("7dc9d5f9-9ed9-44ec-9bbf-0600bb589fbb")
	mftCategoryVideoEncoder   = guid("f79eac7d-e545-4387-bdee-d647d7bde42a")
	mfMediaTypeVideo          = guid("73646976-0000-0010-8000-00aa00389b71")
	mfVideoFormatNV12         = guid("3231564e-0000-0010-8000-00aa00389b71")
	mfVideoFormatH264         = guid("34363248-0000-0010-8000-00aa00389b71")
	mfMTMajorType             = guid("48eba18e-f8c9-4687-bf11-0a74c9f96a8f")
	mfMTSubtype               = guid("f7e34c9a-42e8-4714-b74b-cb29d72c35e5")
	mfMTFrameSize             = guid("1652c33d-d6b2-4012-b834-72030849a37d")
	mfMTFrameRate             = guid("c459a2e8-3d2c-4e44-b132-fee5156c7bb0")
	mfMTAvgBitrate            = guid("20332624-fb0d-4d9e-bd0d-cbf6786c102e")
	mfMTInterlaceMode         = guid("e2724bb8-e676-4806-b4b2-a8d6efb44ccd")
	mfMTPixelAspectRatio      = guid("c6376a1e-8d0a-4027-be45-6d9a0ad39bb6")
	mfMTMpeg2Profile          = guid("ad76a80b-2d5c-4e0b-b375-64e520137036")
	mfMTVideoNominalRange     = guid("c21b8ee5-b956-4071-8daf-325edf5cab11")
	mfMTYUVMatrix             = guid("3e23d450-2c75-4d25-a00e-b91670d12327")
	mfMTVideoPrimaries        = guid("dbfbe4d7-0740-4ee0-8192-850ab0e21935")
	mfMTTransferFunction      = guid("5fb0fce9-be5c-4935-a811-ec838f8eed93")
	mfTransformAsync          = guid("f81a699a-649a-497d-8c73-29f8fed6ad7a")
	mfTransformAsyncUnlock    = guid("e5666d6b-3422-4eb6-a421-da7db1f8e207")
	mftEnumAdapterLUID        = guid("1d39518c-e220-4da8-a07f-ba172552d6b1")
	mftFriendlyName           = guid("314ffbae-5b41-4c95-9c19-4e7d586face3")
	mfSampleCleanPoint        = guid("9cdf01d8-a0f0-43ba-b077-eaa06cbd728a")
	codecRateControlMode      = guid("1c0608e9-370c-4710-8a58-cb6181c42423")
	codecMeanBitRate          = guid("f7222374-2144-4815-b550-a37f8e12ee52")
	codecBufferSize           = guid("0db96574-b6a4-4c8b-8106-3773de0310cd")
	codecLowLatencyMode       = guid("9c27891a-ed7a-40e1-88e8-b22727a024ee")
	codecGOPSize              = guid("95f31b26-95a4-41aa-9303-246a7fc6eef1")
	codecForceKeyFrame        = guid("398c1b98-8353-475a-9ef2-8f265d260345")
	codecBPictureCount        = guid("8d390aac-dc5c-4200-b57f-814d04babab2")
	codecCABAC                = guid("ee6cad62-d305-4248-a50e-e1b255f7caf8")
	codecQualityVsSpeed       = guid("98332df8-03cd-476b-89fa-3f9e442dec9f")
)

// vtable slots
const (
	attrGetUINT32        = 7
	attrGetUINT64        = 8
	attrGetString        = 12
	attrGetStringLength  = 11
	attrSetUINT32        = 21
	attrSetUINT64        = 22
	attrSetGUID          = 24
	activateObject       = 33
	activateShutdown     = 34
	mediaTypeGetMajor    = 33
	eventGetType         = 33
	eventGetStatus       = 35
	sampleSetTime        = 36
	sampleGetTime        = 35
	sampleSetDuration    = 38
	sampleToContiguous   = 41
	sampleAddBuffer      = 42
	bufferLock           = 3
	bufferUnlock         = 4
	bufferSetLength      = 6
	buffer2DContigLength = 7
	dxgiManagerReset     = 7
	genGetEvent          = 3
	codecSetValue        = 9

	mftGetOutputStreamInfo   = 7
	mftGetAttributes         = 8
	mftGetInputAvailableType = 13
	mftGetOutputAvailType    = 14
	mftSetInputType          = 15
	mftSetOutputType         = 16
	mftProcessMessage        = 23
	mftProcessInput          = 24
	mftProcessOutput         = 25

	mftMessageCommandFlush     = 0
	mftMessageCommandDrain     = 1
	mftMessageSetD3DManager    = 2
	mftMessageBeginStreaming   = 0x10000000
	mftMessageEndStreaming     = 0x10000001
	mftMessageEndOfStream      = 0x10000002
	mftMessageStartOfStream    = 0x10000003
	meTransformNeedInput       = 601
	meTransformHaveOutput      = 602
	meTransformDrainComplete   = 603
	mftOutputProvidesSamples   = 0x100
	mftOutputCanProvideSamples = 0x200
	mfETransformStreamChange   = hresult(-1072861855) // 0xC00D6D61
	mfETransformNeedMoreInput  = hresult(-1072861838) // 0xC00D6D72
	eAVEncH264VProfileHigh     = 100
	rateControlCBR             = 0
)

// variant is a VARIANT as ICodecAPI takes it.
type variant struct {
	vt  uint16
	_   [3]uint16
	val uint64
	_   uint64
}

const (
	vtUI4  = 19
	vtBool = 11
)

func mfStartup() error {
	return procCall(procMFStartup, 0x20070 /* MF_VERSION */, 0 /* MFSTARTUP_FULL */).err("MFStartup")
}

func setU32(attrs unsafe.Pointer, key *windows.GUID, v uint32) error {
	return comCall(attrs, attrSetUINT32, uintptr(unsafe.Pointer(key)), uintptr(v)).err("SetUINT32")
}

func setU64(attrs unsafe.Pointer, key *windows.GUID, v uint64) error {
	return comCall(attrs, attrSetUINT64, uintptr(unsafe.Pointer(key)), uintptr(v)).err("SetUINT64")
}

func setGUID(attrs unsafe.Pointer, key, v *windows.GUID) error {
	return comCall(attrs, attrSetGUID, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(v))).err("SetGUID")
}

func getU32(attrs unsafe.Pointer, key *windows.GUID) (uint32, bool) {
	var v uint32
	return v, comCall(attrs, attrGetUINT32, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v))) >= 0
}

func getU64(attrs unsafe.Pointer, key *windows.GUID) (uint64, bool) {
	var v uint64
	return v, comCall(attrs, attrGetUINT64, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v))) >= 0
}

func getString(attrs unsafe.Pointer, key *windows.GUID) string {
	var n uint32
	if comCall(attrs, attrGetStringLength, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&n))) < 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	if comCall(attrs, attrGetString, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&buf[0])), uintptr(n+1), 0) < 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// encodedFrame is one access unit out of the encoder, in Annex-B.
type encodedFrame struct {
	data     []byte
	keyframe bool
	pts      time.Duration // the time the frame went in with
	took     time.Duration // from going in to coming out
}

// mfEncoder is one hardware H.264 encoder running on the shared device.
type mfEncoder struct {
	name          string
	mft, codec    unsafe.Pointer // IMFTransform, ICodecAPI
	events        unsafe.Pointer // IMFMediaEventGenerator
	manager       unsafe.Pointer // IMFDXGIDeviceManager
	activate      unsafe.Pointer
	provides      bool   // the encoder makes its own output samples
	outSize       uint32 // otherwise, how large ours must be
	width, height int
	fps           int

	credits  atomic.Int32  // inputs the encoder asked for and did not get yet
	asked    chan struct{} // wakes whoever waits for the encoder to want a frame
	bitrate  atomic.Uint32
	mu       sync.Mutex
	inFlight map[int64]time.Time // pts (100 ns) -> when it went in
	out      func(encodedFrame)
	done     chan struct{}
	err      atomic.Pointer[error]
	sps, pps []byte // the last parameter sets, put before an IDR that comes without them
}

// hardwareEncoders lists the hardware H.264 encoders that take NV12, best first; free releases
// the list.
func hardwareEncoders() ([]unsafe.Pointer, func(), error) {
	in := [2]windows.GUID{mfMediaTypeVideo, mfVideoFormatNV12}
	out := [2]windows.GUID{mfMediaTypeVideo, mfVideoFormatH264}
	var list unsafe.Pointer
	var count uint32
	const flags = 0x4 | 0x40 // MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER
	if err := procCall(procMFTEnumEx, uintptr(unsafe.Pointer(&mftCategoryVideoEncoder)), flags,
		uintptr(unsafe.Pointer(&in)), uintptr(unsafe.Pointer(&out)), uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&count))).err("MFTEnumEx"); err != nil {
		return nil, nil, err
	}
	if count == 0 {
		procCoTaskMemFree.Call(uintptr(list))
		return nil, nil, errors.New("no hardware H.264 encoder")
	}
	acts := unsafe.Slice((*unsafe.Pointer)(list), count)
	return acts, func() {
		for _, a := range acts {
			release(a)
		}
		procCoTaskMemFree.Call(uintptr(list))
	}, nil
}

// newMFEncoder opens the first hardware H.264 encoder of the device's adapter that takes NV12
// textures at this size. out receives every frame, on the encoder's event goroutine.
func newMFEncoder(d *device, width, height, fps, bitrateKbps int, out func(encodedFrame)) (*mfEncoder, error) {
	if err := mfStartup(); err != nil {
		return nil, err
	}
	acts, free, err := hardwareEncoders()
	if err != nil {
		return nil, err
	}
	defer free()
	var errs []error
	for _, a := range acts {
		name := getString(a, &mftFriendlyName)
		if luid, ok := getU64(a, &mftEnumAdapterLUID); ok && d.adapterLUID != 0 && luid != d.adapterLUID {
			continue // an encoder of another GPU (a laptop with two)
		}
		e, err := openMFEncoder(d, a, name, width, height, fps, bitrateKbps, out)
		if err == nil {
			return e, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", name, err))
	}
	if len(errs) == 0 {
		return nil, errors.New("no hardware H.264 encoder on " + d.adapterName)
	}
	return nil, errors.Join(errs...)
}

func openMFEncoder(d *device, act unsafe.Pointer, name string, width, height, fps, bitrateKbps int, out func(encodedFrame)) (*mfEncoder, error) {
	e := &mfEncoder{name: name, width: width, height: height, fps: fps, out: out, inFlight: make(map[int64]time.Time),
		asked: make(chan struct{}, 1), done: make(chan struct{})}
	if err := comCall(act, activateObject, uintptr(unsafe.Pointer(&iidIMFTransform)), uintptr(unsafe.Pointer(&e.mft))).err("ActivateObject"); err != nil {
		return nil, err
	}
	addRef(act)
	e.activate = act
	fail := func(err error) (*mfEncoder, error) { e.close(); return nil, err }

	var attrs unsafe.Pointer
	if err := comCall(e.mft, mftGetAttributes, uintptr(unsafe.Pointer(&attrs))).err("GetAttributes"); err != nil {
		return fail(err)
	}
	async, _ := getU32(attrs, &mfTransformAsync)
	if async != 0 {
		_ = setU32(attrs, &mfTransformAsyncUnlock, 1)
	}
	release(attrs)
	if async == 0 {
		return fail(errors.New("not an asynchronous encoder"))
	}
	var err error
	if e.events, err = queryInterface(e.mft, &iidIMFMediaEventGenerator, "IMFMediaEventGenerator"); err != nil {
		return fail(err)
	}
	var token uint32
	if err := procCall(procMFCreateDXGIDeviceManager, uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&e.manager))).err("MFCreateDXGIDeviceManager"); err != nil {
		return fail(err)
	}
	if err := comCall(e.manager, dxgiManagerReset, uintptr(d.dev), uintptr(token)).err("ResetDevice"); err != nil {
		return fail(err)
	}
	if err := comCall(e.mft, mftProcessMessage, mftMessageSetD3DManager, uintptr(e.manager)).err("SET_D3D_MANAGER"); err != nil {
		return fail(err)
	}

	if e.codec, err = queryInterface(e.mft, &iidICodecAPI, "ICodecAPI"); err != nil {
		return fail(err)
	}
	bps := uint32(bitrateKbps * 1000)
	e.bitrate.Store(bps)
	// What the stream needs: low delay, a steady rate the network can plan for, keyframes only when
	// asked (a viewer joins or lost something), no B-frames (WebRTC decoders do not reorder).
	// The buffer holds about six frames, so a keyframe is a few frames' worth of bits, not a second's.
	e.setCodec(&codecLowLatencyMode, vtBool, 0xFFFF)
	e.setCodec(&codecRateControlMode, vtUI4, rateControlCBR)
	e.setCodec(&codecMeanBitRate, vtUI4, uint64(bps))
	e.setCodec(&codecBufferSize, vtUI4, uint64(bps)/uint64(max(fps, 1))*6)
	e.setCodec(&codecGOPSize, vtUI4, uint64(fps*600))
	e.setCodec(&codecBPictureCount, vtUI4, 0)
	e.setCodec(&codecCABAC, vtBool, 0xFFFF)
	e.setCodec(&codecQualityVsSpeed, vtUI4, 33)

	if err := e.setTypes(); err != nil {
		return fail(err)
	}
	var info struct{ Flags, Size, Alignment uint32 }
	if err := comCall(e.mft, mftGetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&info))).err("GetOutputStreamInfo"); err != nil {
		return fail(err)
	}
	e.provides = info.Flags&(mftOutputProvidesSamples|mftOutputCanProvideSamples) != 0
	e.outSize = max(info.Size, uint32(width*height*3/2))
	if err := comCall(e.mft, mftProcessMessage, mftMessageBeginStreaming, 0).err("BEGIN_STREAMING"); err != nil {
		return fail(err)
	}
	if err := comCall(e.mft, mftProcessMessage, mftMessageStartOfStream, 0).err("START_OF_STREAM"); err != nil {
		return fail(err)
	}
	go e.eventLoop()
	return e, nil
}

func (e *mfEncoder) setCodec(key *windows.GUID, vt uint16, val uint64) bool {
	v := variant{vt: vt, val: val}
	return comCall(e.codec, codecSetValue, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v))) >= 0
}

func (e *mfEncoder) videoType(subtype *windows.GUID) (unsafe.Pointer, error) {
	var t unsafe.Pointer
	if err := procCall(procMFCreateMediaType, uintptr(unsafe.Pointer(&t))).err("MFCreateMediaType"); err != nil {
		return nil, err
	}
	_ = setGUID(t, &mfMTMajorType, &mfMediaTypeVideo)
	_ = setGUID(t, &mfMTSubtype, subtype)
	e.describe(t)
	return t, nil
}

// describe puts size, rate and color on a media type: BT.709, limited range, as the converter
// makes it, so the encoder can say so in the stream.
func (e *mfEncoder) describe(t unsafe.Pointer) {
	_ = setU64(t, &mfMTFrameSize, uint64(e.width)<<32|uint64(e.height))
	_ = setU64(t, &mfMTFrameRate, uint64(e.fps)<<32|1)
	_ = setU64(t, &mfMTPixelAspectRatio, 1<<32|1)
	_ = setU32(t, &mfMTInterlaceMode, 2) // progressive
	_ = setU32(t, &mfMTVideoNominalRange, 2)
	_ = setU32(t, &mfMTYUVMatrix, 1)
	_ = setU32(t, &mfMTVideoPrimaries, 2)
	_ = setU32(t, &mfMTTransferFunction, 5)
}

func (e *mfEncoder) setTypes() error {
	ot, err := e.videoType(&mfVideoFormatH264)
	if err != nil {
		return err
	}
	_ = setU32(ot, &mfMTAvgBitrate, e.bitrate.Load())
	_ = setU32(ot, &mfMTMpeg2Profile, eAVEncH264VProfileHigh)
	err = comCall(e.mft, mftSetOutputType, 0, uintptr(ot), 0).err("SetOutputType")
	release(ot)
	if err != nil {
		return err
	}
	// the input type the encoder offers for NV12, with our size on it
	for i := 0; ; i++ {
		var it unsafe.Pointer
		if err := comCall(e.mft, mftGetInputAvailableType, 0, uintptr(i), uintptr(unsafe.Pointer(&it))).err("GetInputAvailableType"); err != nil {
			return fmt.Errorf("no NV12 input: %w", err)
		}
		var sub windows.GUID
		comCall(it, 10 /* GetGUID */, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&sub)))
		if sub != mfVideoFormatNV12 {
			release(it)
			continue
		}
		e.describe(it)
		err := comCall(e.mft, mftSetInputType, 0, uintptr(it), 0).err("SetInputType")
		release(it)
		return err
	}
}

// ready reports whether the encoder asked for a frame it did not get yet.
func (e *mfEncoder) ready() bool { return e.credits.Load() > 0 }

// encode hands the encoder an NV12 texture. It must not be written until its frame comes out.
func (e *mfEncoder) encode(tex unsafe.Pointer, pts time.Duration, keyframe bool) error {
	if e.credits.Add(-1) < 0 {
		e.credits.Add(1)
		return errors.New("the encoder did not ask for a frame")
	}
	var buf unsafe.Pointer
	if err := procCall(procMFCreateDXGISurfaceBuffer, uintptr(unsafe.Pointer(&iidID3D11Texture2D)), uintptr(tex), 0, 0, uintptr(unsafe.Pointer(&buf))).err("MFCreateDXGISurfaceBuffer"); err != nil {
		return err
	}
	defer release(buf)
	if b2, err := queryInterface(buf, &iidIMF2DBuffer, "IMF2DBuffer"); err == nil {
		var n uint32
		if comCall(b2, buffer2DContigLength, uintptr(unsafe.Pointer(&n))) >= 0 {
			comCall(buf, bufferSetLength, uintptr(n))
		}
		release(b2)
	}
	var sample unsafe.Pointer
	if err := procCall(procMFCreateSample, uintptr(unsafe.Pointer(&sample))).err("MFCreateSample"); err != nil {
		return err
	}
	defer release(sample)
	if err := comCall(sample, sampleAddBuffer, uintptr(buf)).err("AddBuffer"); err != nil {
		return err
	}
	t100 := int64(pts / 100)
	comCall(sample, sampleSetTime, uintptr(t100))
	comCall(sample, sampleSetDuration, uintptr(int64(time.Second/100)/int64(max(e.fps, 1))))
	if keyframe {
		e.setCodec(&codecForceKeyFrame, vtUI4, 1)
	}
	e.mu.Lock()
	e.inFlight[t100] = time.Now()
	e.mu.Unlock()
	if err := comCall(e.mft, mftProcessInput, 0, uintptr(sample), 0).err("ProcessInput"); err != nil {
		e.mu.Lock()
		delete(e.inFlight, t100)
		e.mu.Unlock()
		return err
	}
	return nil
}

// setBitrate changes the rate on the fly; small changes are not worth the encoder's time.
func (e *mfEncoder) setBitrate(bps uint32) {
	cur := e.bitrate.Load()
	if cur != 0 && bps > cur*97/100 && bps < cur*103/100 {
		return
	}
	if e.setCodec(&codecMeanBitRate, vtUI4, uint64(bps)) {
		e.bitrate.Store(bps)
	}
}

func (e *mfEncoder) eventLoop() {
	defer close(e.done)
	undo, err := mtaThread()
	if err != nil {
		e.fail(err)
		return
	}
	defer undo()
	for {
		var ev unsafe.Pointer
		if err := comCall(e.events, genGetEvent, 0, uintptr(unsafe.Pointer(&ev))).err("GetEvent"); err != nil {
			e.fail(err)
			return
		}
		var typ uint32
		var status hresult
		comCall(ev, eventGetType, uintptr(unsafe.Pointer(&typ)))
		comCall(ev, eventGetStatus, uintptr(unsafe.Pointer(&status)))
		release(ev)
		if err := status.err("encoder event"); err != nil {
			e.fail(err)
			return
		}
		switch typ {
		case meTransformNeedInput:
			e.credits.Add(1)
			select {
			case e.asked <- struct{}{}:
			default:
			}
		case meTransformHaveOutput:
			if err := e.output(); err != nil {
				e.fail(err)
				return
			}
		case meTransformDrainComplete:
			return
		}
	}
}

func (e *mfEncoder) fail(err error) { e.err.CompareAndSwap(nil, &err) }

// failed returns the error that stopped the encoder, if any.
func (e *mfEncoder) failed() error {
	if p := e.err.Load(); p != nil {
		return *p
	}
	return nil
}

func (e *mfEncoder) output() error {
	var buf struct {
		StreamID uint32
		Sample   unsafe.Pointer
		Status   uint32
		Events   unsafe.Pointer
	}
	if !e.provides {
		var mb unsafe.Pointer
		if err := procCall(procMFCreateMemoryBuffer, uintptr(e.outSize), uintptr(unsafe.Pointer(&mb))).err("MFCreateMemoryBuffer"); err != nil {
			return err
		}
		if err := procCall(procMFCreateSample, uintptr(unsafe.Pointer(&buf.Sample))).err("MFCreateSample"); err != nil {
			release(mb)
			return err
		}
		comCall(buf.Sample, sampleAddBuffer, uintptr(mb))
		release(mb)
	}
	var status uint32
	hr := comCall(e.mft, mftProcessOutput, 0, 1, uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&status)))
	release(buf.Events)
	defer release(buf.Sample)
	switch {
	case hr == mfETransformStreamChange:
		// the encoder wants its output type again (some do this once, before the first frame)
		var t unsafe.Pointer
		if err := comCall(e.mft, mftGetOutputAvailType, 0, 0, uintptr(unsafe.Pointer(&t))).err("GetOutputAvailableType"); err != nil {
			return err
		}
		defer release(t)
		return comCall(e.mft, mftSetOutputType, 0, uintptr(t), 0).err("SetOutputType after a stream change")
	case hr == mfETransformNeedMoreInput:
		return nil
	case hr < 0:
		return hr.err("ProcessOutput")
	}
	if buf.Sample == nil {
		return nil
	}
	var t100 int64
	comCall(buf.Sample, sampleGetTime, uintptr(unsafe.Pointer(&t100)))
	clean, _ := getU32(buf.Sample, &mfSampleCleanPoint)
	var mb unsafe.Pointer
	if err := comCall(buf.Sample, sampleToContiguous, uintptr(unsafe.Pointer(&mb))).err("ConvertToContiguousBuffer"); err != nil {
		return err
	}
	defer release(mb)
	var data unsafe.Pointer
	var maxLen, curLen uint32
	if err := comCall(mb, bufferLock, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&curLen))).err("Lock"); err != nil {
		return err
	}
	au := append([]byte(nil), unsafe.Slice((*byte)(data), curLen)...)
	comCall(mb, bufferUnlock)

	e.mu.Lock()
	in, ok := e.inFlight[t100]
	for k := range e.inFlight {
		if k <= t100 {
			delete(e.inFlight, k)
		}
	}
	e.mu.Unlock()
	f := encodedFrame{data: e.withParameterSets(au), keyframe: clean != 0, pts: time.Duration(t100) * 100}
	if ok {
		f.took = time.Since(in)
	}
	f.keyframe = f.keyframe || hasIDR(f.data)
	e.out(f)
	return nil
}

// withParameterSets remembers SPS and PPS and puts them before an IDR that comes without them, so
// a viewer that joins on any keyframe can decode it.
func (e *mfEncoder) withParameterSets(au []byte) []byte {
	var sps, pps, idr bool
	for _, n := range annexBNALs(au) {
		switch n[0] & 0x1f {
		case 7:
			e.sps, sps = append(e.sps[:0], n...), true
		case 8:
			e.pps, pps = append(e.pps[:0], n...), true
		case 5:
			idr = true
		}
	}
	if !idr || (sps && pps) || e.sps == nil || e.pps == nil {
		return au
	}
	out := make([]byte, 0, len(au)+len(e.sps)+len(e.pps)+8)
	out = append(out, 0, 0, 0, 1)
	out = append(out, e.sps...)
	out = append(out, 0, 0, 0, 1)
	out = append(out, e.pps...)
	return append(out, au...)
}

// drain stops the encoder: what it holds comes out, then its event loop ends.
func (e *mfEncoder) drain(timeout time.Duration) {
	comCall(e.mft, mftProcessMessage, mftMessageEndOfStream, 0)
	if comCall(e.mft, mftProcessMessage, mftMessageCommandDrain, 0) < 0 {
		return
	}
	select {
	case <-e.done:
	case <-time.After(timeout):
	}
}

func (e *mfEncoder) close() {
	if e.mft != nil {
		comCall(e.mft, mftProcessMessage, mftMessageCommandFlush, 0)
		comCall(e.mft, mftProcessMessage, mftMessageEndStreaming, 0)
		comCall(e.mft, mftProcessMessage, mftMessageSetD3DManager, 0)
	}
	release(e.codec)
	release(e.events)
	release(e.mft)
	if e.activate != nil {
		comCall(e.activate, activateShutdown)
		release(e.activate)
	}
	release(e.manager)
	e.codec, e.events, e.mft, e.activate, e.manager = nil, nil, nil, nil, nil
}
