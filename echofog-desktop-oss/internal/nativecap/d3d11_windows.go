//go:build windows

package nativecap

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iidID3D11Texture2D     = guid("6f15aaf2-d208-4e89-9ab4-489535d34f9c")
	iidIDXGIDevice         = guid("54ec77fa-1377-44e6-8c32-88fd5f44c84c")
	iidID3D10Multithread   = guid("9b7e4e00-342c-4106-a19f-4f2704f689f0")
	iidID3D11VideoDevice   = guid("10ec4d5b-975a-4689-b9e4-d0aac30fe333")
	iidID3D11VideoContext  = guid("61f21c45-3c0e-4a74-9cea-67100d9ad5e4")
	iidIDirect3DDevice     = guid("a37624ab-8d5f-4650-9d3e-9eae3d9bc670")
	iidIDirect3DDxgiAccess = guid("a9b3d012-3df2-4ee3-b8d1-8695f457d3c1")
)

const (
	dxgiFormatB8G8R8A8 = 87
	dxgiFormatNV12     = 103

	d3d11BindShaderResource = 0x8
	d3d11BindRenderTarget   = 0x20
	d3d11BindVideoEncoder   = 0x400

	d3d11UsageDefault  = 0
	d3d11UsageStaging  = 3
	d3d11CPUAccessRead = 0x20000
)

// vtable slots
const (
	devCreateTexture2D        = 5
	devGetDeviceRemovedReason = 39
	ctxMap                    = 14
	ctxUnmap                  = 15
	ctxCopySubresourceRegion  = 46
	ctxCopyResource           = 47
	ctxFlush                  = 111
	texGetDesc                = 10
	dxgiDevGetAdapter         = 7
	dxgiAdapterGetDesc        = 8
	mtSetMultithreadProtected = 5

	vdevCreateVideoProcessor           = 4
	vdevCreateVideoProcessorInputView  = 8
	vdevCreateVideoProcessorOutputView = 9
	vdevCreateVideoProcessorEnumerator = 10
	vctxSetOutputTargetRect            = 13
	vctxSetOutputBackgroundColor       = 14
	vctxSetOutputColorSpace            = 15
	vctxGetOutputColorSpace            = 22
	vctxSetStreamFrameFormat           = 27
	vctxSetStreamColorSpace            = 28
	vctxSetStreamSourceRect            = 30
	vctxSetStreamDestRect              = 31
	vctxSetStreamAutoProcessingMode    = 37
	vctxGetStreamSourceRect            = 43
	vctxVideoProcessorBlt              = 53
)

type texture2DDesc struct {
	Width, Height, MipLevels, ArraySize uint32
	Format                              uint32
	SampleCount, SampleQuality          uint32
	Usage, BindFlags, CPUAccessFlags    uint32
	MiscFlags                           uint32
}

type rect struct{ Left, Top, Right, Bottom int32 }

// device is the D3D11 device everything runs on: capture, conversion and the encoder.
type device struct {
	dev, ctx      unsafe.Pointer // ID3D11Device, ID3D11DeviceContext
	vdev, vctx    unsafe.Pointer // ID3D11VideoDevice, ID3D11VideoContext
	winrt         unsafe.Pointer // IDirect3DDevice for Windows.Graphics.Capture
	adapterName   string
	adapterLUID   uint64
	adapterVendor uint32
}

func newDevice() (*device, error) {
	d := &device{}
	levels := []uint32{0xb100, 0xb000} // 11.1, 11.0
	const flags = 0x20 | 0x800         // BGRA_SUPPORT | VIDEO_SUPPORT
	var fl uint32
	hr := procCall(procD3D11CreateDevice, 0, 1 /* HARDWARE */, 0, flags,
		uintptr(unsafe.Pointer(&levels[0])), uintptr(len(levels)), 7, /* D3D11_SDK_VERSION */
		uintptr(unsafe.Pointer(&d.dev)), uintptr(unsafe.Pointer(&fl)), uintptr(unsafe.Pointer(&d.ctx)))
	if err := hr.err("D3D11CreateDevice"); err != nil {
		return nil, err
	}
	fail := func(err error) (*device, error) { d.close(); return nil, err }

	// the encoder uses the device from its own threads
	mt, err := queryInterface(d.dev, &iidID3D10Multithread, "ID3D10Multithread")
	if err != nil {
		return fail(err)
	}
	comCall(mt, mtSetMultithreadProtected, 1)
	release(mt)

	if d.vdev, err = queryInterface(d.dev, &iidID3D11VideoDevice, "ID3D11VideoDevice"); err != nil {
		return fail(err)
	}
	if d.vctx, err = queryInterface(d.ctx, &iidID3D11VideoContext, "ID3D11VideoContext"); err != nil {
		return fail(err)
	}

	dxgi, err := queryInterface(d.dev, &iidIDXGIDevice, "IDXGIDevice")
	if err != nil {
		return fail(err)
	}
	defer release(dxgi)
	var adapter unsafe.Pointer
	if err := comCall(dxgi, dxgiDevGetAdapter, uintptr(unsafe.Pointer(&adapter))).err("IDXGIDevice.GetAdapter"); err == nil {
		var desc struct {
			Description                        [128]uint16
			VendorID, DeviceID, SubSysID, Rev  uint32
			Dedicated, DedicatedSys, SharedSys uintptr
			LUIDLow                            uint32
			LUIDHigh                           int32
		}
		if comCall(adapter, dxgiAdapterGetDesc, uintptr(unsafe.Pointer(&desc))) >= 0 {
			d.adapterName = windows.UTF16ToString(desc.Description[:])
			d.adapterLUID = uint64(desc.LUIDLow) | uint64(uint32(desc.LUIDHigh))<<32
			d.adapterVendor = desc.VendorID
		}
		release(adapter)
	}
	var insp unsafe.Pointer
	if err := procCall(procCreateDirect3D11DeviceFromDXGIDevice, uintptr(dxgi), uintptr(unsafe.Pointer(&insp))).err("CreateDirect3D11DeviceFromDXGIDevice"); err != nil {
		return fail(err)
	}
	d.winrt, err = queryInterface(insp, &iidIDirect3DDevice, "IDirect3DDevice")
	release(insp)
	if err != nil {
		return fail(err)
	}
	return d, nil
}

func (d *device) close() {
	for _, p := range []*unsafe.Pointer{&d.winrt, &d.vctx, &d.vdev, &d.ctx, &d.dev} {
		release(*p)
		*p = nil
	}
}

// removed returns why the GPU dropped the device (driver update, reset, unplugged GPU), or nil.
func (d *device) removed() error {
	return hresult(comCall(d.dev, devGetDeviceRemovedReason)).err("device removed")
}

func (d *device) texture(desc texture2DDesc) (unsafe.Pointer, error) {
	if desc.MipLevels == 0 {
		desc.MipLevels = 1
	}
	if desc.ArraySize == 0 {
		desc.ArraySize = 1
	}
	if desc.SampleCount == 0 {
		desc.SampleCount = 1
	}
	var tex unsafe.Pointer
	if err := comCall(d.dev, devCreateTexture2D, uintptr(unsafe.Pointer(&desc)), 0, uintptr(unsafe.Pointer(&tex))).err("CreateTexture2D"); err != nil {
		return nil, fmt.Errorf("%w (%dx%d format %d bind 0x%x)", err, desc.Width, desc.Height, desc.Format, desc.BindFlags)
	}
	return tex, nil
}

func textureDesc(tex unsafe.Pointer) texture2DDesc {
	var desc texture2DDesc
	comCall(tex, texGetDesc, uintptr(unsafe.Pointer(&desc)))
	return desc
}

func (d *device) copyResource(dst, src unsafe.Pointer) {
	comCall(d.ctx, ctxCopyResource, uintptr(dst), uintptr(src))
}

func (d *device) flush() { comCall(d.ctx, ctxFlush) }

// readPixels copies a BGRA texture to the CPU (tests and thumbnails only: it waits for the GPU).
func (d *device) readPixels(tex unsafe.Pointer) ([]byte, int, int, error) {
	desc := textureDesc(tex)
	staging, err := d.texture(texture2DDesc{Width: desc.Width, Height: desc.Height, Format: desc.Format,
		Usage: d3d11UsageStaging, CPUAccessFlags: d3d11CPUAccessRead})
	if err != nil {
		return nil, 0, 0, err
	}
	defer release(staging)
	d.copyResource(staging, tex)
	var mapped struct {
		Data       unsafe.Pointer
		RowPitch   uint32
		DepthPitch uint32
	}
	if err := comCall(d.ctx, ctxMap, uintptr(staging), 0, 1 /* D3D11_MAP_READ */, 0, uintptr(unsafe.Pointer(&mapped))).err("Map"); err != nil {
		return nil, 0, 0, err
	}
	defer comCall(d.ctx, ctxUnmap, uintptr(staging), 0)
	w, h := int(desc.Width), int(desc.Height)
	out := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		row := unsafe.Slice((*byte)(unsafe.Add(mapped.Data, uintptr(y)*uintptr(mapped.RowPitch))), w*4)
		copy(out[y*w*4:], row)
	}
	return out, w, h, nil
}

// converter turns captured BGRA frames into NV12 frames of a fixed size on the GPU's video
// engine: scaled down to fit, the rest black, BT.709 limited range as H.264 expects. With rgb it
// makes BGRA frames instead (thumbnails).
type converter struct {
	d          *device
	enum, proc unsafe.Pointer // ID3D11VideoProcessorEnumerator, ID3D11VideoProcessor
	inW, inH   uint32         // the input texture the enumerator was made for
	outW, outH uint32
	inputs     map[unsafe.Pointer]unsafe.Pointer // input texture -> its input view
}

// Color spaces as D3D11_VIDEO_PROCESSOR_COLOR_SPACE bits: Usage, RGB_Range, YCbCr_Matrix,
// YCbCr_xvYCC, Nominal_Range (2 bits).
const (
	colorSpaceRGBFull     = 0           // RGB 0-255
	colorSpaceBT709Studio = 1<<2 | 1<<4 // BT.709 matrix, 16-235
)

func newConverter(d *device, inW, inH, outW, outH uint32, rgb bool) (*converter, error) {
	c := &converter{d: d, inW: inW, inH: inH, outW: outW, outH: outH, inputs: make(map[unsafe.Pointer]unsafe.Pointer)}
	desc := struct {
		InputFrameFormat          uint32
		InNum, InDen              uint32
		InputWidth, InputHeight   uint32
		OutNum, OutDen            uint32
		OutputWidth, OutputHeight uint32
		Usage                     uint32
	}{0, 60, 1, inW, inH, 60, 1, outW, outH, 0}
	if err := comCall(d.vdev, vdevCreateVideoProcessorEnumerator, uintptr(unsafe.Pointer(&desc)), uintptr(unsafe.Pointer(&c.enum))).err("CreateVideoProcessorEnumerator"); err != nil {
		return nil, err
	}
	if err := comCall(d.vdev, vdevCreateVideoProcessor, uintptr(c.enum), 0, uintptr(unsafe.Pointer(&c.proc))).err("CreateVideoProcessor"); err != nil {
		c.close()
		return nil, err
	}
	in, out := uint32(colorSpaceRGBFull), uint32(colorSpaceBT709Studio)
	if rgb {
		out = colorSpaceRGBFull
	}
	comCall(d.vctx, vctxSetStreamColorSpace, uintptr(c.proc), 0, uintptr(unsafe.Pointer(&in)))
	comCall(d.vctx, vctxSetOutputColorSpace, uintptr(c.proc), uintptr(unsafe.Pointer(&out)))
	comCall(d.vctx, vctxSetStreamFrameFormat, uintptr(c.proc), 0, 0) // progressive
	// no driver "enhancements": they sharpen text and smear motion
	comCall(d.vctx, vctxSetStreamAutoProcessingMode, uintptr(c.proc), 0, 0)
	black := [4]float32{0, 0, 0, 1}
	comCall(d.vctx, vctxSetOutputBackgroundColor, uintptr(c.proc), 0, uintptr(unsafe.Pointer(&black)))
	full := rect{0, 0, int32(outW), int32(outH)}
	comCall(d.vctx, vctxSetOutputTargetRect, uintptr(c.proc), 1, uintptr(unsafe.Pointer(&full)))

	// The slots above have no return value to check; a read back proves the table is right.
	var got uint32
	comCall(d.vctx, vctxGetOutputColorSpace, uintptr(c.proc), uintptr(unsafe.Pointer(&got)))
	if got&0x3f != out {
		c.close()
		return nil, fmt.Errorf("video processor did not take the output color space (0x%x)", got)
	}
	return c, nil
}

func (c *converter) close() {
	for _, v := range c.inputs {
		release(v)
	}
	c.inputs = nil
	release(c.proc)
	release(c.enum)
	c.proc, c.enum = nil, nil
}

// outputView makes the render target view of an NV12 texture of the converter's size.
func (c *converter) outputView(nv12 unsafe.Pointer) (unsafe.Pointer, error) {
	desc := struct {
		ViewDimension uint32
		MipSlice      uint32
		_             [2]uint32
	}{ViewDimension: 1}
	var view unsafe.Pointer
	if err := comCall(c.d.vdev, vdevCreateVideoProcessorOutputView, uintptr(nv12), uintptr(c.enum), uintptr(unsafe.Pointer(&desc)), uintptr(unsafe.Pointer(&view))).err("CreateVideoProcessorOutputView"); err != nil {
		return nil, err
	}
	return view, nil
}

func (c *converter) inputView(tex unsafe.Pointer) (unsafe.Pointer, error) {
	if v, ok := c.inputs[tex]; ok {
		return v, nil
	}
	desc := struct {
		FourCC        uint32
		ViewDimension uint32
		MipSlice      uint32
		ArraySlice    uint32
	}{ViewDimension: 1}
	var view unsafe.Pointer
	if err := comCall(c.d.vdev, vdevCreateVideoProcessorInputView, uintptr(tex), uintptr(c.enum), uintptr(unsafe.Pointer(&desc)), uintptr(unsafe.Pointer(&view))).err("CreateVideoProcessorInputView"); err != nil {
		return nil, err
	}
	c.inputs[tex] = view
	return view, nil
}

// convert scales the w x h top-left part of src (a BGRA texture of the input size) into the NV12
// texture behind out, keeping its aspect ratio.
func (c *converter) convert(src unsafe.Pointer, w, h uint32, out unsafe.Pointer) error {
	in, err := c.inputView(src)
	if err != nil {
		return err
	}
	w, h = min(w, c.inW), min(h, c.inH)
	if w == 0 || h == 0 {
		return errors.New("empty frame")
	}
	srcRect := rect{0, 0, int32(w), int32(h)}
	dst := fitRect(w, h, c.outW, c.outH)
	comCall(c.d.vctx, vctxSetStreamSourceRect, uintptr(c.proc), 0, 1, uintptr(unsafe.Pointer(&srcRect)))
	comCall(c.d.vctx, vctxSetStreamDestRect, uintptr(c.proc), 0, 1, uintptr(unsafe.Pointer(&dst)))
	stream := struct {
		Enable, OutputIndex, InputFrameOrField, PastFrames, FutureFrames uint32
		_                                                                uint32
		PastSurfaces, InputSurface, FutureSurfaces                       unsafe.Pointer
		PastSurfacesRight, InputSurfaceRight, FutureSurfacesRight        unsafe.Pointer
	}{Enable: 1, InputSurface: in}
	return comCall(c.d.vctx, vctxVideoProcessorBlt, uintptr(c.proc), uintptr(out), 0, 1, uintptr(unsafe.Pointer(&stream))).err("VideoProcessorBlt")
}

// fitRect places a w x h picture in an outW x outH frame as large as it fits, centered.
func fitRect(w, h, outW, outH uint32) rect {
	if w == 0 || h == 0 {
		return rect{0, 0, int32(outW), int32(outH)}
	}
	dw, dh := outW, uint32(uint64(h)*uint64(outW)/uint64(w))
	if dh > outH {
		dw, dh = uint32(uint64(w)*uint64(outH)/uint64(h)), outH
	}
	dw, dh = dw&^1, dh&^1
	x, y := (outW-dw)/2&^1, (outH-dh)/2&^1
	return rect{int32(x), int32(y), int32(x + dw), int32(y + dh)}
}

// textureFromSurface returns the ID3D11Texture2D behind a WinRT IDirect3DSurface.
func textureFromSurface(surface unsafe.Pointer) (unsafe.Pointer, error) {
	access, err := queryInterface(surface, &iidIDirect3DDxgiAccess, "IDirect3DDxgiInterfaceAccess")
	if err != nil {
		return nil, err
	}
	defer release(access)
	var tex unsafe.Pointer
	if err := comCall(access, 3, uintptr(unsafe.Pointer(&iidID3D11Texture2D)), uintptr(unsafe.Pointer(&tex))).err("GetInterface(ID3D11Texture2D)"); err != nil {
		return nil, err
	}
	return tex, nil
}
