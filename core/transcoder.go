package core

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"github.com/golang/glog"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/livepeer/go-livepeer/clog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/monitor"
	"github.com/livepeer/lpms/ffmpeg"
)

type Transcoder interface {
	Transcode(ctx context.Context, md *SegTranscodingMetadata) (*TranscodeData, error)
}

type LocalTranscoder struct {
	workDir string
}

type UnrecoverableError struct {
	error
}

func NewUnrecoverableError(err error) UnrecoverableError {
	return UnrecoverableError{err}
}

var WorkDir string

func (lt *LocalTranscoder) Transcode(ctx context.Context, md *SegTranscodingMetadata) (td *TranscodeData, retErr error) {
	// Returns UnrecoverableError instead of panicking to gracefully notify orchestrator about transcoder's failure
	defer recoverFromPanic(&retErr)

	// Set up in / out config
	in := &ffmpeg.TranscodeOptionsIn{
		Fname: md.Fname,
		Accel: ffmpeg.Software,
	}
	profiles := md.Profiles
	opts := profilesToTranscodeOptions(lt.workDir, ffmpeg.Software, profiles, md.CalcPerceptualHash)
	if md.DetectorEnabled {
		opts = append(opts, detectorsToTranscodeOptions(lt.workDir, ffmpeg.Software, md.DetectorProfiles)...)
	}

	_, seqNo, parseErr := parseURI(md.Fname)
	start := time.Now()

	res, err := ffmpeg.Transcode3(in, opts)
	if err != nil {
		return nil, err
	}

	if monitor.Enabled && parseErr == nil {
		// This will run only when fname is actual URL and contains seqNo in it.
		// When orchestrator works as transcoder, `fname` will be relative path to file in local
		// filesystem and will not contain seqNo in it. For that case `SegmentTranscoded` will
		// be called in orchestrator.go
		monitor.SegmentTranscoded(ctx, 0, seqNo, md.Duration, time.Since(start), common.ProfilesNames(profiles), true, true)
	}

	return resToTranscodeData(ctx, res, opts)
}

func NewLocalTranscoder(workDir string) Transcoder {
	return &LocalTranscoder{workDir: workDir}
}

type NvidiaTranscoder struct {
	device  string
	session *ffmpeg.Transcoder
}

func (nv *NvidiaTranscoder) Transcode(ctx context.Context, md *SegTranscodingMetadata) (td *TranscodeData, retErr error) {
	// Returns UnrecoverableError instead of panicking to gracefully notify orchestrator about transcoder's failure
	defer recoverFromPanic(&retErr)

	in := &ffmpeg.TranscodeOptionsIn{
		Fname:  md.Fname,
		Accel:  ffmpeg.Nvidia,
		Device: nv.device,
	}
	profiles := md.Profiles
	out := profilesToTranscodeOptions(WorkDir, ffmpeg.Nvidia, profiles, md.CalcPerceptualHash)
	if md.DetectorEnabled {
		out = append(out, detectorsToTranscodeOptions(WorkDir, ffmpeg.Nvidia, md.DetectorProfiles)...)
	}

	_, seqNo, parseErr := parseURI(md.Fname)
	start := time.Now()

	res, err := nv.session.Transcode(in, out)
	if err != nil {
		return nil, err
	}

	if monitor.Enabled && parseErr == nil {
		// This will run only when fname is actual URL and contains seqNo in it.
		// When orchestrator works as transcoder, `fname` will be relative path to file in local
		// filesystem and will not contain seqNo in it. For that case `SegmentTranscoded` will
		// be called in orchestrator.go
		monitor.SegmentTranscoded(ctx, 0, seqNo, md.Duration, time.Since(start), common.ProfilesNames(profiles), true, true)
	}

	return resToTranscodeData(ctx, res, out)
}

type transcodeTestParams struct {
	TestAvailable bool
	Cap           Capability
	OutProfile    ffmpeg.VideoProfile
	SegmentPath   string
}

func (params transcodeTestParams) IsRequired() bool {
	return InArray(params.Cap, DefaultCapabilities())
}

func (params transcodeTestParams) Kind() string {
	if params.IsRequired() {
		return "required capability"
	}
	return "optional capability"
}

func (params transcodeTestParams) Name() string {
	name, err := CapabilityToName(params.Cap)
	if err == nil {
		return name
	}
	return "unknown"
}

type continueLoop bool

func forEachTranscoderSample(handler func(*transcodeTestParams) continueLoop) {
	// default capabilities
	allCaps := append(DefaultCapabilities(), OptionalCapabilities()...)
	handlerParams := transcodeTestParams{SegmentPath: filepath.Join(WorkDir, "testseg.tempfile")}
	defer os.Remove(handlerParams.SegmentPath)
	for _, handlerParams.Cap = range allCaps {
		var capTest CapabilityTest
		capTest, handlerParams.TestAvailable = CapabilityTestLookup[handlerParams.Cap]
		if handlerParams.TestAvailable {
			handlerParams.OutProfile = capTest.outProfile
			b := bytes.NewReader(capTest.inVideoData)
			z, err := gzip.NewReader(b)
			if err != nil {
				continue
			}
			mp4testSeg, err := ioutil.ReadAll(z)
			z.Close()
			if err != nil {
				glog.Errorf("error reading test segment for capability %d: %s", handlerParams.Cap, err)
				continue
			}
			err = ioutil.WriteFile(handlerParams.SegmentPath, mp4testSeg, 0644)
			if err != nil {
				glog.Errorf("error writing test segment for capability %d: %s", handlerParams.Cap, err)
				continue
			}
		}
		if !handler(&handlerParams) {
			return
		}
	}
}

func testNvidiaTranscode(device string, fname string, profile ffmpeg.VideoProfile, renditionCount int) (outputProduced, outputValid bool, err error) {
	transcoder := NewNvidiaTranscoder(device)
	outputProfiles := make([]ffmpeg.VideoProfile, 0, renditionCount)
	for i := 0; i < renditionCount; i++ {
		outputProfiles = append(outputProfiles, profile)
	}
	metadata := &SegTranscodingMetadata{Fname: fname, Profiles: outputProfiles}
	td, err := transcoder.Transcode(context.Background(), metadata)
	transcoder.Stop()
	if err != nil {
		return false, false, err
	}
	outputProduced = len(td.Segments) > 0
	outputValid = td.Pixels > 0
	return outputProduced, outputValid, err
}

// Test which capabilities transcoder supports
func TestTranscoderCapabilities(devices []string) (caps []Capability, fatalError error) {
	fatalError = nil
	forEachTranscoderSample(func(params *transcodeTestParams) continueLoop {
		if !params.TestAvailable {
			// Assume capability is supported if we do not have test for it
			caps = append(caps, params.Cap)
			return true
		}
		runRestrictedSessionTest := true
		transcodingFailed := func() {
			// check GeForce limit
			if runRestrictedSessionTest {
				// do it only once
				runRestrictedSessionTest = false
				// if 4 renditions didn't succeed, try 3 renditions on first device to check if it could be session limit
				outputProduced, outputValid, err := testNvidiaTranscode(devices[0], params.SegmentPath, params.OutProfile, 3)
				if err != nil && outputProduced && outputValid {
					glog.Error("Maximum number of simultaneous NVENC video encoding sessions is restricted by driver")
					fatalError = fmt.Errorf("maximum number of simultaneous NVENC video encoding sessions is restricted by driver")
				}
			}
			if params.IsRequired() {
				// All devices need to support this capability, stop further testing
				fatalError = fmt.Errorf("%s %q is not supported on hardware", params.Kind(), params.Name())
			}
		}
		// check that capability is supported on all devices
		for _, device := range devices {
			outputProduced, outputValid, err := testNvidiaTranscode(device, params.SegmentPath, params.OutProfile, 4)
			if err != nil {
				glog.Infof("%s %q is not supported on device %s, see other error messages for details", params.Kind(), params.Name(), device)
				// likely means capability is not supported, don't check on other devices
				transcodingFailed()
				return fatalError == nil
			}
			if !outputProduced || !outputValid {
				// abnormal behavior
				glog.Errorf("Empty result segment when testing for %s %q", params.Kind(), params.Name())
				transcodingFailed()
				return fatalError == nil
			}
			// no error creating 4 renditions - disable 3 renditions test, as restriction is on driver level, not device
			runRestrictedSessionTest = false
		}
		caps = append(caps, params.Cap)
		return true
	})
	return caps, fatalError
}

func testSoftwareTranscode(tmpdir string, fname string, profile ffmpeg.VideoProfile, renditionCount int) (outputProduced, outputValid bool, err error) {
	transcoder := NewLocalTranscoder(tmpdir)
	outputProfiles := make([]ffmpeg.VideoProfile, 0, renditionCount)
	for i := 0; i < renditionCount; i++ {
		outputProfiles = append(outputProfiles, profile)
	}
	metadata := &SegTranscodingMetadata{Fname: fname, Profiles: outputProfiles}
	td, err := transcoder.Transcode(context.Background(), metadata)
	if err != nil {
		return false, false, err
	}
	outputProduced = len(td.Segments) > 0
	outputValid = td.Pixels > 0
	return outputProduced, outputValid, err
}

func TestSoftwareTranscoderCapabilities(tmpdir string) (caps []Capability, fatalError error) {
	// iterate all capabilities and test ones which has test data
	fatalError = nil
	forEachTranscoderSample(func(params *transcodeTestParams) continueLoop {
		if !params.TestAvailable {
			caps = append(caps, params.Cap)
			return true
		}
		// check that capability is supported on all devices
		outputProduced, outputValid, err := testSoftwareTranscode(tmpdir, params.SegmentPath, params.OutProfile, 4)
		if err != nil {
			// likely means capability is not supported
			return true
		}
		if !outputProduced || !outputValid {
			// abnormal behavior
			fatalError = fmt.Errorf("empty result segment when testing for capability %d", params.Cap)
			return false
		}
		caps = append(caps, params.Cap)
		return true
	})
	return caps, fatalError
}

func NewNvidiaTranscoder(gpu string) TranscoderSession {
	return &NvidiaTranscoder{
		device:  gpu,
		session: ffmpeg.NewTranscoder(),
	}
}

func NewNvidiaTranscoderWithDetector(detector ffmpeg.DetectorProfile, gpu string) (TranscoderSession, error) {
	// Hardcode detection to device 0 for now
	// Transcoding can still run on a separate GPU as we copy frames to CPU before detection
	session, err := ffmpeg.NewTranscoderWithDetector(detector, gpu)
	return &NvidiaTranscoder{
		device:  gpu,
		session: session,
	}, err
}

func (nv *NvidiaTranscoder) Stop() {
	nv.session.StopTranscoder()
}

func parseURI(uri string) (string, uint64, error) {
	var mid string
	var seqNo uint64
	parts := strings.Split(uri, "/")
	if len(parts) < 3 {
		return mid, seqNo, fmt.Errorf("BadURI")
	}
	mid = parts[len(parts)-2]
	parts = strings.Split(parts[len(parts)-1], ".")
	seqNo, err := strconv.ParseUint(parts[0], 10, 64)
	return mid, seqNo, err
}

func resToTranscodeData(ctx context.Context, res *ffmpeg.TranscodeResults, opts []ffmpeg.TranscodeOptions) (*TranscodeData, error) {
	if len(res.Encoded) != len(opts) {
		return nil, errors.New("lengths of results and options different")
	}

	// Convert results into in-memory bytes following the expected API
	segments := []*TranscodedSegmentData{}
	// Extract detection data from detector outputs
	detections := []ffmpeg.DetectData{}
	for i := range opts {
		if opts[i].Detector == nil {
			oname := opts[i].Oname
			o, err := ioutil.ReadFile(oname)
			if err != nil {
				clog.Errorf(ctx, "Cannot read transcoded output for name=%s", oname)
				return nil, err
			}
			// Extract perceptual hash if calculated
			var s []byte = nil
			if opts[i].CalcSign {
				sigfile := oname + ".bin"
				s, err = ioutil.ReadFile(sigfile)
				if err != nil {
					clog.Errorf(ctx, "Cannot read perceptual hash at name=%s", sigfile)
					return nil, err
				}
				err = os.Remove(sigfile)
				if err != nil {
					clog.Errorf(ctx, "Cannot delete perceptual hash after reading name=%s", sigfile)
				}
			}
			segments = append(segments, &TranscodedSegmentData{Data: o, Pixels: res.Encoded[i].Pixels, PHash: s})
			os.Remove(oname)
		} else {
			detections = append(detections, res.Encoded[i].DetectData)
		}
	}

	return &TranscodeData{
		Segments:   segments,
		Pixels:     res.Decoded.Pixels,
		Detections: detections,
	}, nil
}

func profilesToTranscodeOptions(workDir string, accel ffmpeg.Acceleration, profiles []ffmpeg.VideoProfile, calcPHash bool) []ffmpeg.TranscodeOptions {
	opts := make([]ffmpeg.TranscodeOptions, len(profiles))
	for i := range profiles {
		o := ffmpeg.TranscodeOptions{
			Oname:        fmt.Sprintf("%s/out_%s.tempfile", workDir, common.RandName()),
			Profile:      profiles[i],
			Accel:        accel,
			AudioEncoder: ffmpeg.ComponentOptions{Name: "copy"},
			CalcSign:     calcPHash,
		}
		opts[i] = o
	}
	return opts
}

func detectorsToTranscodeOptions(workDir string, accel ffmpeg.Acceleration, profiles []ffmpeg.DetectorProfile) []ffmpeg.TranscodeOptions {
	opts := make([]ffmpeg.TranscodeOptions, len(profiles))
	for i := range profiles {
		var o ffmpeg.TranscodeOptions
		switch profiles[i].Type() {
		case ffmpeg.SceneClassification:
			classifier := profiles[i].(*ffmpeg.SceneClassificationProfile)
			classifier.ModelPath = ffmpeg.DSceneAdultSoccer.ModelPath
			classifier.Input = ffmpeg.DSceneAdultSoccer.Input
			classifier.Output = ffmpeg.DSceneAdultSoccer.Output
			o = ffmpeg.TranscodeOptions{
				Detector: classifier,
				Accel:    accel,
			}
		}
		opts[i] = o
	}
	return opts
}

func recoverFromPanic(retErr *error) {
	if r := recover(); r != nil {
		err, ok := r.(error)
		if !ok {
			err = errors.New("unrecoverable transcoding failure")
		}
		*retErr = NewUnrecoverableError(err)
	}
}
