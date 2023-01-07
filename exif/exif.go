// Package exif implements decoding of EXIF data as defined in the EXIF 2.2
// specification (http://www.exif.org/Exif2-2.PDF).
package exif

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/goexif2/tiff"
)

const (
	jpeg_APP1 = 0xE1
	jpeg_COM  = 0xFE

	exifPointer    = 0x8769
	gpsPointer     = 0x8825
	interopPointer = 0xA005
)

// A decodeError is returned when the image cannot be decoded as a tiff image.
type decodeError struct {
	cause error
}

func (de decodeError) Error() string {
	return fmt.Sprintf("exif: decode failed (%v) ", de.cause.Error())
}

// IsShortReadTagValueError identifies a ErrShortReadTagValue error.
func IsShortReadTagValueError(err error) bool {
	de, ok := err.(decodeError)
	if ok {
		return de.cause == tiff.ErrShortReadTagValue
	}
	return false
}

var flashDescriptions = map[int]string{
	0x0:  "No Flash",
	0x1:  "Fired",
	0x5:  "Fired, Return not detected",
	0x7:  "Fired, Return detected",
	0x8:  "On, Did not fire",
	0x9:  "On, Fired",
	0xD:  "On, Return not detected",
	0xF:  "On, Return detected",
	0x10: "Off, Did not fire",
	0x14: "Off, Did not fire, Return not detected",
	0x18: "Auto, Did not fire",
	0x19: "Auto, Fired",
	0x1D: "Auto, Fired, Return not detected",
	0x1F: "Auto, Fired, Return detected",
	0x20: "No flash function",
	0x30: "Off, No flash function",
	0x41: "Fired, Red-eye reduction",
	0x45: "Fired, Red-eye reduction, Return not detected",
	0x47: "Fired, Red-eye reduction, Return detected",
	0x49: "On, Red-eye reduction",
	0x4D: "On, Red-eye reduction, Return not detected",
	0x4F: "On, Red-eye reduction, Return detected",
	0x50: "Off, Red-eye reduction",
	0x58: "Auto, Did not fire, Red-eye reduction",
	0x59: "Auto, Fired, Red-eye reduction",
	0x5D: "Auto, Fired, Red-eye reduction, Return not detected",
	0x5F: "Auto, Fired, Red-eye reduction, Return detected",
}

// A TagNotPresentError is returned when the requested field is not
// present in the EXIF.
type TagNotPresentError FieldName

func (tag TagNotPresentError) Error() string {
	return fmt.Sprintf("exif: tag %q is not present", string(tag))
}

func IsTagNotPresentError(err error) bool {
	_, ok := err.(TagNotPresentError)
	return ok
}

// Parser allows the registration of custom parsing and field loading
// in the Decode function.
type Parser interface {
	// Parse should read data from x and insert parsed fields into x via
	// LoadTags.
	Parse(x *Exif) error
}

var parsers []Parser

func init() {
	RegisterParsers(&parser{})
}

// RegisterParsers registers one or more parsers to be automatically called
// when decoding EXIF data via the Decode function.
func RegisterParsers(ps ...Parser) {
	parsers = append(parsers, ps...)
}

type parser struct{}

type tiffErrors map[tiffError]string

func (te tiffErrors) Error() string {
	var allErrors []string
	for k, v := range te {
		allErrors = append(allErrors, fmt.Sprintf("%s: %v\n", stagePrefix[k], v))
	}
	return strings.Join(allErrors, "\n")
}

// IsCriticalError, given the error returned by Decode, reports whether the
// returned *Exif may contain usable information.
func IsCriticalError(err error) bool {
	_, ok := err.(tiffErrors)
	return !ok
}

// IsExifError reports whether the error happened while decoding the EXIF
// sub-IFD.
func IsExifError(err error) bool {
	if te, ok := err.(tiffErrors); ok {
		_, isExif := te[loadExif]
		return isExif
	}
	return false
}

// IsGPSError reports whether the error happened while decoding the GPS sub-IFD.
func IsGPSError(err error) bool {
	if te, ok := err.(tiffErrors); ok {
		_, isGPS := te[loadExif]
		return isGPS
	}
	return false
}

// IsInteroperabilityError reports whether the error happened while decoding the
// Interoperability sub-IFD.
func IsInteroperabilityError(err error) bool {
	if te, ok := err.(tiffErrors); ok {
		_, isInterop := te[loadInteroperability]
		return isInterop
	}
	return false
}

type tiffError int

const (
	loadExif tiffError = iota
	loadGPS
	loadInteroperability
)

var stagePrefix = map[tiffError]string{
	loadExif:             "loading EXIF sub-IFD",
	loadGPS:              "loading GPS sub-IFD",
	loadInteroperability: "loading Interoperability sub-IFD",
}

// Parse reads data from the tiff data in x and populates the tags
// in x. If parsing a sub-IFD fails, the error is recorded and
// parsing continues with the remaining sub-IFDs.
func (p *parser) Parse(x *Exif) error {
	if len(x.Tiff.Dirs) == 0 {
		return errors.New("Invalid exif data")
	}
	x.LoadTags(x.Tiff.Dirs[0], exifFields, false)

	// thumbnails
	if len(x.Tiff.Dirs) >= 2 {
		x.LoadTags(x.Tiff.Dirs[1], thumbnailFields, false)
	}

	te := make(tiffErrors)

	// recurse into exif, gps, and interop sub-IFDs
	if err := loadSubDir(x, ExifIFDPointer, exifFields); err != nil {
		te[loadExif] = err.Error()
	}
	if err := loadSubDir(x, GPSInfoIFDPointer, gpsFields); err != nil {
		te[loadGPS] = err.Error()
	}

	if err := loadSubDir(x, InteroperabilityIFDPointer, interopFields); err != nil {
		te[loadInteroperability] = err.Error()
	}
	if len(te) > 0 {
		return te
	}
	return nil
}

func loadSubDir(x *Exif, ptr FieldName, fieldMap map[uint16]FieldName) error {
	r := bytes.NewReader(x.Raw)

	tag, err := x.Get(ptr)
	if err != nil {
		return nil
	}
	offset, err := tag.Int64(0)
	if err != nil {
		return nil
	}

	_, err = r.Seek(offset, 0)
	if err != nil {
		return fmt.Errorf("exif: seek to sub-IFD %s failed: %v", ptr, err)
	}
	subDir, _, err := tiff.DecodeDir(r, x.Tiff.Order)
	if err != nil {
		return fmt.Errorf("exif: sub-IFD %s decode failed: %v", ptr, err)
	}
	x.LoadTags(subDir, fieldMap, false)
	return nil
}

// Exif provides access to decoded EXIF metadata fields and values.
type Exif struct {
	Tiff *tiff.Tiff
	main map[FieldName]*tiff.Tag
	Raw  []byte
	// Contents of the JPEG COM segment (Comment).
	Comment string
}

// Decode parses EXIF-encoded data from r and returns a queryable Exif
// object. After the exif data section is called and the tiff structure
// decoded, each registered parser is called (in order of registration). If
// one parser returns an error, decoding terminates and the remaining
// parsers are not called.
// The error can be inspected with functions such as IsCriticalError to
// determine whether the returned object might still be usable.
func Decode(r io.Reader) (*Exif, error) {
	// EXIF data in JPEG is stored in the APP1 marker. EXIF data uses the TIFF
	// format to store data.
	// If we're parsing a TIFF image, we don't need to strip away any data.
	// If we're parsing a JPEG image, we need to strip away the JPEG APP1
	// marker and also the EXIF header.

	header := make([]byte, 16)
	_, err := io.ReadFull(r, header)
	if err != nil {
		return nil, err
	}

	var isTiff bool
	var isHEIC bool
	switch {
	case bytes.HasPrefix(header, []byte("II*\x00")),
		bytes.HasPrefix(header, []byte("MM\x00*")):
		// TIFF - Little endian (Intel)
		// TIFF - Big endian (Motorola)
		isTiff = true
	case bytes.HasPrefix(header[4:], []byte("ftyp")):
		prefix := header[8:]
		isHEIC = bytes.HasPrefix(prefix, []byte("mif1")) ||
			bytes.HasPrefix(prefix, []byte("msf1")) ||
			bytes.HasPrefix(prefix, []byte("heic")) ||
			bytes.HasPrefix(prefix, []byte("heix")) ||
			bytes.HasPrefix(prefix, []byte("hevc")) ||
			bytes.HasPrefix(prefix, []byte("hevx"))
	default:
		// Not TIFF, assume JPEG
	}

	// Put the header bytes back into the reader.
	r = io.MultiReader(bytes.NewReader(header), r)
	var (
		er  *bytes.Reader
		tif *tiff.Tiff
	)

	var comment string
	if isTiff {
		// Functions below need the IFDs from the TIFF data to be stored in a
		// *bytes.Reader.  We use TeeReader to get a copy of the bytes as a
		// side-effect of tiff.Decode() doing its work.
		b := &bytes.Buffer{}
		tr := io.TeeReader(r, b)
		tif, err = tiff.Decode(tr)
		er = bytes.NewReader(b.Bytes())
	} else if isHEIC {
		var hr io.Reader
		var hbuf []byte
		hr, err = newHEICReader(r)
		if err != nil {
			return nil, err
		}
		hbuf, err = ioutil.ReadAll(hr)
		if err != nil {
			return nil, err
		}
		er = bytes.NewReader(hbuf)
		tif, err = tiff.Decode(er)
	} else {
		// Locate the JPEG APP1 header.
		var sec *appSec
		sec, err = newAppSec(jpeg_APP1, r)
		if err != nil {
			return nil, err
		}
		var desc *appSec
		desc, err = newAppSec(jpeg_COM, r)
		if err == nil {
			comment = string(desc.data)
		}
		// Strip away EXIF header.
		er, err = sec.exifReader()
		if err != nil {
			return nil, err
		}
		tif, err = tiff.Decode(er)
	}
	if err != nil {
		return nil, decodeError{cause: err}
	}

	er.Seek(0, 0)
	raw, err := ioutil.ReadAll(er)
	if err != nil {
		return nil, decodeError{cause: err}
	}

	// build an exif structure from the tiff
	x := &Exif{
		main:    map[FieldName]*tiff.Tag{},
		Tiff:    tif,
		Raw:     raw,
		Comment: comment,
	}

	for i, p := range parsers {
		if err := p.Parse(x); err != nil {
			if _, ok := err.(tiffErrors); ok {
				return x, err
			}
			// This should never happen, as Parse always returns a tiffError
			// for now, but that could change.
			return x, fmt.Errorf("exif: parser %v failed (%v)", i, err)
		}
	}

	return x, nil
}

// LoadTags loads tags into the available fields from the tiff Directory
// using the given tagid-fieldname mapping.  Used to load makernote and
// other meta-data.  If showMissing is true, tags in d that are not in the
// fieldMap will be loaded with the FieldName UnknownPrefix followed by the
// tag ID (in hex format).
func (x *Exif) LoadTags(d *tiff.Dir, fieldMap map[uint16]FieldName, showMissing bool) {
	for _, tag := range d.Tags {
		name := fieldMap[tag.Id]
		if name == "" {
			if !showMissing {
				continue
			}
			name = FieldName(fmt.Sprintf("%v%x", UnknownPrefix, tag.Id))
		}
		x.main[name] = tag
	}
}

// Get retrieves the EXIF tag for the given field name.
//
// If the tag is not known or not present, an error is returned. If the
// tag name is known, the error will be a TagNotPresentError.
func (x *Exif) Get(name FieldName) (*tiff.Tag, error) {
	if tg, ok := x.main[name]; ok {
		return tg, nil
	}
	return nil, TagNotPresentError(name)
}

// Walker is the interface used to traverse all fields of an Exif object.
type Walker interface {
	// Walk is called for each non-nil EXIF field. Returning a non-nil
	// error aborts the walk/traversal.
	Walk(name FieldName, tag *tiff.Tag) error
}

// Walk calls the Walk method of w with the name and tag for every non-nil
// EXIF field.  If w aborts the walk with an error, that error is returned.
func (x *Exif) Walk(w Walker) error {
	for name, tag := range x.main {
		if err := w.Walk(name, tag); err != nil {
			return err
		}
	}
	return nil
}

// DateTime returns the EXIF's "DateTimeOriginal" field, which
// is the creation time of the photo. If not found, it tries
// the "DateTime" (which is meant as the modtime) instead.
// The error will be TagNotPresentErr if none of those tags
// were found, or a generic error if the tag value was
// not a string, or the error returned by time.Parse.
//
// If the EXIF lacks timezone information or GPS time, the returned
// time's Location will be time.Local.
func (x *Exif) DateTime() (time.Time, error) {
	var dt time.Time
	tag, err := x.Get(DateTimeOriginal)
	if err != nil {
		tag, err = x.Get(DateTime)
		if err != nil {
			return dt, err
		}
	}
	tagVal, err := tag.StringVal()
	if err != nil {
		return dt, err
	}
	exifTimeLayout := "2006:01:02 15:04:05"
	dateStr := strings.TrimRight(tagVal, "\x00")
	// TODO(bradfitz,mpl): look for timezone offset, GPS time, etc.
	timeZone := time.Local
	if tz, _ := x.TimeZone(); tz != nil {
		timeZone = tz
	}
	return time.ParseInLocation(exifTimeLayout, dateStr, timeZone)
}

// Check various EXIF fields for a timezone offset
func (x *Exif) TimeZone() (*time.Location, error) {
	// TimeZoneOffset
	timeOffset, err := x.Get(TimeZoneOffset)
	if err == nil {
		offset, err := timeOffset.Int(0)
		if err != nil {
			return nil, err
		}
		if offset > 24 {
			offset -= 65536
		}
		if offset < -24 {
			return nil, errors.New("Invalid timezone offset")
		}
		label := fmt.Sprintf("UTC%+d", offset)
		return time.FixedZone(label, offset*60*60), nil
	}
	// Other common model fields
	timeInfo, err := x.Get("Canon.TimeInfo")
	if err == nil {
		if timeInfo.Count < 2 {
			return nil, errors.New("Canon.TimeInfo does not contain timezone")
		}
		offsetMinutes, err := timeInfo.Int(1)
		if err != nil {
			return nil, err
		}
		return time.FixedZone("", offsetMinutes*60), nil
	}
	// TODO: parse more timezone fields (e.g. Nikon WorldTime).
	return nil, errors.New("No time zone infomation found")
}

func ratFloat(num, dem int64) float64 {
	return float64(num) / float64(dem)
}

// Tries to parse a Geo degrees value from a string as it was found in some
// EXIF data.
// Supported formats so far:
//   - "52,00000,50,00000,34,01180" ==> 52 deg 50'34.0118"
//     Probably due to locale the comma is used as decimal mark as well as the
//     separator of three floats (degrees, minutes, seconds)
//     http://en.wikipedia.org/wiki/Decimal_mark#Hindu.E2.80.93Arabic_numeral_system
//   - "52.0,50.0,34.01180" ==> 52deg50'34.0118"
//   - "52,50,34.01180"     ==> 52deg50'34.0118"
func parseTagDegreesString(s string) (float64, error) {
	const unparsableErrorFmt = "Unknown coordinate format: %s"
	isSplitRune := func(c rune) bool {
		return c == ',' || c == ';'
	}
	parts := strings.FieldsFunc(s, isSplitRune)
	var degrees, minutes, seconds float64
	var err error
	switch len(parts) {
	case 6:
		degrees, err = strconv.ParseFloat(parts[0]+"."+parts[1], 64)
		if err != nil {
			return 0.0, fmt.Errorf(unparsableErrorFmt, s)
		}
		minutes, err = strconv.ParseFloat(parts[2]+"."+parts[3], 64)
		if err != nil {
			return 0.0, fmt.Errorf(unparsableErrorFmt, s)
		}
		minutes = math.Copysign(minutes, degrees)
		seconds, err = strconv.ParseFloat(parts[4]+"."+parts[5], 64)
		if err != nil {
			return 0.0, fmt.Errorf(unparsableErrorFmt, s)
		}
		seconds = math.Copysign(seconds, degrees)
	case 3:
		degrees, err = strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0.0, fmt.Errorf(unparsableErrorFmt, s)
		}
		minutes, err = strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0.0, fmt.Errorf(unparsableErrorFmt, s)
		}
		minutes = math.Copysign(minutes, degrees)
		seconds, err = strconv.ParseFloat(parts[2], 64)
		if err != nil {
			return 0.0, fmt.Errorf(unparsableErrorFmt, s)
		}
		seconds = math.Copysign(seconds, degrees)
	default:
		return 0.0, fmt.Errorf(unparsableErrorFmt, s)
	}
	return degrees + minutes/60.0 + seconds/3600.0, nil
}

func parse3Rat2(tag *tiff.Tag) ([3]float64, error) {
	v := [3]float64{}
	for i := range v {
		num, den, err := tag.Rat2(i)
		if err != nil {
			return v, err
		}
		v[i] = ratFloat(num, den)
		if tag.Count < uint32(i+2) {
			break
		}
	}
	return v, nil
}

func tagDegrees(tag *tiff.Tag) (float64, error) {
	switch tag.Format() {
	case tiff.RatVal:
		// The usual case, according to the Exif spec
		// (http://www.kodak.com/global/plugins/acrobat/en/service/digCam/exifStandard2.pdf,
		// sec 4.6.6, p. 52 et seq.)
		v, err := parse3Rat2(tag)
		if err != nil {
			return 0.0, err
		}
		return v[0] + v[1]/60 + v[2]/3600.0, nil
	case tiff.StringVal:
		// Encountered this weird case with a panorama picture taken with a HTC phone
		s, err := tag.StringVal()
		if err != nil {
			return 0.0, err
		}
		return parseTagDegreesString(s)
	default:
		// don't know how to parse value, give up
		return 0.0, fmt.Errorf("Malformed EXIF Tag Degrees")
	}
}

// LatLong returns the latitude and longitude of the photo and
// whether it was present.
func (x *Exif) LatLong() (lat, long float64, err error) {
	// All calls of x.Get might return an TagNotPresentError
	longTag, err := x.Get(FieldName("GPSLongitude"))
	if err != nil {
		return
	}
	ewTag, err := x.Get(FieldName("GPSLongitudeRef"))
	if err != nil {
		return
	}
	latTag, err := x.Get(FieldName("GPSLatitude"))
	if err != nil {
		return
	}
	nsTag, err := x.Get(FieldName("GPSLatitudeRef"))
	if err != nil {
		return
	}
	if long, err = tagDegrees(longTag); err != nil {
		return 0, 0, fmt.Errorf("Cannot parse longitude: %v", err)
	}
	if lat, err = tagDegrees(latTag); err != nil {
		return 0, 0, fmt.Errorf("Cannot parse latitude: %v", err)
	}
	if math.Abs(long) > 180.0 {
		return 0, 0, fmt.Errorf("Longitude outside allowed range: %v", long)
	}
	if math.Abs(lat) > 90.0 {
		return 0, 0, fmt.Errorf("Latitude outside allowed range: %v", lat)
	}
	ew, err := ewTag.StringVal()
	if err == nil && ew == "W" {
		long *= -1.0
	} else if err != nil {
		return 0, 0, fmt.Errorf("Cannot parse longitude: %v", err)
	}
	ns, err := nsTag.StringVal()
	if err == nil && ns == "S" {
		lat *= -1.0
	} else if err != nil {
		return 0, 0, fmt.Errorf("Cannot parse longitude: %v", err)
	}
	return lat, long, nil
}

// String returns a pretty text representation of the decoded exif data.
func (x *Exif) String() string {
	var buf bytes.Buffer
	for name, tag := range x.main {
		fmt.Fprintf(&buf, "%s: %s\n", name, tag)
	}
	return buf.String()
}

// JpegThumbnail returns the jpeg thumbnail if it exists. If it doesn't exist,
// TagNotPresentError will be returned
func (x *Exif) JpegThumbnail() ([]byte, error) {
	return x.getBytesFromTagOffsets(ThumbJPEGInterchangeFormat, ThumbJPEGInterchangeFormatLength)
}

// PreviewImage returns the preview image if it exists. If it doesn't exist,
// TagNotPresentError will be returned
func (x *Exif) PreviewImage() ([]byte, error) {
	return x.getBytesFromTagOffsets(PreviewImageStart, PreviewImageLength)
}

// JpegFromRaw returns the jpeg from raw image if it exists. If it doesn't exist,
// TagNotPresentError will be returned
func (x *Exif) JpegFromRaw() ([]byte, error) {
	return x.getBytesFromTagOffsets(JpegFromRawFormat, JpegFromRawFormatLength)
}

// getBytesFromTagOffsets returns the bytes specified by the given start and length tag, if they exist.
func (x *Exif) getBytesFromTagOffsets(startTag, lengthTag FieldName) ([]byte, error) {
	offset, err := x.Get(startTag)
	if err != nil {
		return nil, err
	}
	start, err := offset.Int(0)
	if err != nil {
		return nil, err
	}

	length, err := x.Get(lengthTag)
	if err != nil {
		return nil, err
	}
	l, err := length.Int(0)
	if err != nil {
		return nil, err
	}

	return x.Raw[start : start+l], nil
}

// MarshalJson implements the encoding/json.Marshaler interface providing output of
// all EXIF fields present (names and values).
func (x Exif) MarshalJSON() ([]byte, error) {
	return json.Marshal(x.main)
}

type appSec struct {
	marker byte
	data   []byte
}

// newAppSec finds marker in r and returns the corresponding application data
// section.
func newAppSec(marker byte, r io.Reader) (*appSec, error) {
	br := bufio.NewReader(r)
	app := &appSec{marker: marker}
	var dataLen int

	// seek to marker
	for dataLen == 0 {
		if _, err := br.ReadBytes(0xFF); err != nil {
			return nil, err
		}
		c, err := br.ReadByte()
		if err != nil {
			return nil, err
		} else if c != marker {
			continue
		}

		dataLenBytes := make([]byte, 2)
		for k, _ := range dataLenBytes {
			c, err := br.ReadByte()
			if err != nil {
				return nil, err
			}
			dataLenBytes[k] = c
		}
		dataLen = int(binary.BigEndian.Uint16(dataLenBytes)) - 2
	}

	// read section data
	nread := 0
	for nread < dataLen {
		s := make([]byte, dataLen-nread)
		n, err := br.Read(s)
		nread += n
		if err != nil && nread < dataLen {
			return nil, err
		}
		app.data = append(app.data, s[:n]...)
	}
	return app, nil
}

// reader returns a reader on this appSec.
func (app *appSec) reader() *bytes.Reader {
	return bytes.NewReader(app.data)
}

// exifReader returns a reader on this appSec with the read cursor advanced to
// the start of the exif's tiff encoded portion.
func (app *appSec) exifReader() (*bytes.Reader, error) {
	if len(app.data) < 6 {
		return nil, errors.New("exif: failed to find exif intro marker")
	}
	// read/check for exif special mark
	exif := app.data[:6]
	if !bytes.Equal(exif, append([]byte("Exif"), 0x00, 0x00)) {
		return nil, errors.New("exif: failed to find exif intro marker")
	}
	return bytes.NewReader(app.data[6:]), nil
}

// Flash returns the descriptive text that corresponds to the flash value of the
// photo if it is present.
func (x *Exif) Flash() (string, error) {
	flashTag, err := x.Get(FieldName("Flash"))
	if err != nil {
		return "", err
	}
	flashVal, err := flashTag.Int(0)
	if err != nil {
		return "", err
	}
	return flashDescriptions[flashVal], nil
}

func newHEICReader(r io.Reader) (io.Reader, error) {
	bufr := bufio.NewReader(io.LimitReader(r, 5*1024*1024))
	offr := newOffsetReader(bufr)
	extentOffset, extentLength, extentFound, err := metaBox(offr)
	if err != nil {
		return nil, err
	}
	if !extentFound {
		return nil, errors.New("exif: could not find extent in heic file")
	}
	exifReader, err := offr.OffsetReader(extentOffset + 10)
	if err != nil {
		return nil, err
	}
	return io.LimitReader(exifReader, int64(extentLength)), nil
}

func metaBox(r io.Reader) (extentOffset, extentLength int, extentFound bool, err error) {
	var exifItemID uint16
	var exifItemFound bool
	for {
		var b *box
		b, err = readBox(r)
		if err == io.EOF {
			err = nil
			return
		}
		if err != nil || b.size <= 0 {
			return
		}
		switch b.name {
		case "meta":
			// discard 4 bytes fullbox header
			if _, err = discard(b, 4); err != nil {
				return
			}
			return metaBox(b)
		case "iinf":
			exifItemID, exifItemFound, err = parseIINF(b)
			if err != nil {
				return
			}
			if !exifItemFound {
				err = errors.New("exif: no exif item in iinf box")
				return
			}
		case "iloc":
			if !exifItemFound {
				err = errors.New("exif: no exif item in iinf box")
				return
			}
			return parseILOC(b, exifItemID)
		default:
			_, err = discard(r, b.size)
			if err != nil {
				return
			}
		}
	}
}

func parseIINF(b *box) (exifItemID uint16, exifItemFound bool, err error) {
	// discard 4 bytes fullbox header
	if _, err = discard(b, 4); err != nil {
		return
	}
	itemCountBuf := make([]byte, 2)
	if _, err = io.ReadFull(b, itemCountBuf); err != nil {
		return
	}
	itemCount := int(binary.BigEndian.Uint16(itemCountBuf))
	for i := 0; i < itemCount; i++ {
		var infe *box
		var infeBuf []byte
		infe, err = readFullBox(b)
		if err != nil {
			return
		}
		if infe.name != "infe" || infe.size < 4 {
			err = errors.New("exif: bad iinf box")
			return
		}
		infeBuf, err = infe.readFull()
		if err != nil {
			return
		}
		if bytes.Contains(infeBuf, []byte{'E', 'x', 'i', 'f'}) {
			exifItemID = binary.BigEndian.Uint16(infeBuf)
			exifItemFound = true
			break
		}
	}
	return
}

func parseILOC(b *box, exifItemID uint16) (extentOffset, extentLength int, extentFound bool, err error) {
	var extentOffsetV interface{}
	var extentLengthV interface{}

	fullHeaderBuf := make([]byte, 4)
	if _, err = io.ReadFull(b, fullHeaderBuf); err != nil {
		return
	}
	version := fullHeaderBuf[0]

	infosBuf := make([]byte, 4)
	if _, err = io.ReadFull(b, infosBuf); err != nil {
		return
	}
	infos := binary.BigEndian.Uint32(infosBuf)
	offsetSize := (infos & 0xF0000000) >> 28
	lengthSize := (infos & 0x0F000000) >> 24
	baseOffsetSize := (infos & 0x00F00000) >> 20
	indexSize := (infos & 0x000F0000) >> 16
	itemCount := (infos & 0x0000FFFF)

	if offsetSize == 0 || lengthSize == 0 {
		err = errors.New("exif: bad iloc box offset/length values")
		return
	}

	var itemHeaderSize uint32
	if version == 1 {
		itemHeaderSize = 4 + 2 + baseOffsetSize + 2
	} else {
		itemHeaderSize = 4 + baseOffsetSize + 2
	}

	var extentSize uint32
	if version == 1 {
		extentSize = (offsetSize + lengthSize + indexSize)
	} else {
		extentSize = (offsetSize + lengthSize)
	}

	itemHeaderBuf := make([]byte, itemHeaderSize)
	for i := 0; i < int(itemCount); i++ {
		if _, err = io.ReadFull(b, itemHeaderBuf); err != nil {
			return
		}

		itemID := binary.BigEndian.Uint16(itemHeaderBuf)
		extentCount := int(binary.BigEndian.Uint16(itemHeaderBuf[itemHeaderSize-2:]))

		if itemID != exifItemID {
			if _, err = discard(b, int(extentSize)*extentCount); err != nil {
				return
			}
		} else {
			for j := 0; j < extentCount; j++ {
				if itemID == exifItemID {
					if version == 1 && indexSize > 0 {
						if _, err = discard(b, int(indexSize)); err != nil {
							return
						}
					}
					switch offsetSize {
					case 4:
						var v uint32
						err = binary.Read(b, binary.BigEndian, &v)
						extentOffsetV = v
					case 8:
						var v uint64
						err = binary.Read(b, binary.BigEndian, &v)
						extentOffsetV = v
					}
					if err != nil {
						return
					}
					switch lengthSize {
					case 4:
						var v uint32
						err = binary.Read(b, binary.BigEndian, &v)
						extentLengthV = v
					case 8:
						var v uint64
						err = binary.Read(b, binary.BigEndian, &v)
						extentLengthV = v
					}
					if err != nil {
						return
					}
					extentFound = true
					switch v := extentOffsetV.(type) {
					case uint32:
						extentOffset = int(v)
					case uint64:
						extentOffset = int(v)
					}
					switch v := extentLengthV.(type) {
					case uint32:
						extentLength = int(v)
					case uint64:
						extentLength = int(v)
					}
					return
				}
			}
		}
	}
	return
}

type discarder interface {
	Discard(n int) (int, error)
}

func discard(r io.Reader, n int) (int, error) {
	switch v := r.(type) {
	case discarder:
		return v.Discard(n)
	default:
		return slowDiscard(r, n)
	}
}

func slowDiscard(r io.Reader, n int) (int, error) {
	buf := make([]byte, n)
	return io.ReadFull(r, buf)
}

type offsetReader struct {
	io.Reader
	b *bytes.Buffer
}

func newOffsetReader(r io.Reader) *offsetReader {
	return &offsetReader{
		Reader: r,
		b:      new(bytes.Buffer),
	}
}

func (r *offsetReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	if n > 0 {
		r.b.Write(p[:n])
	}
	return
}

func (r *offsetReader) OffsetReader(offset int) (io.Reader, error) {
	diff := offset - r.b.Len()
	if diff < 0 {
		b := r.b.Bytes()[offset:]
		return io.MultiReader(bytes.NewReader(b), r.Reader), nil
	}
	if diff > 0 {
		if _, err := discard(r.Reader, diff); err != nil {
			return nil, err
		}
	}
	return r.Reader, nil
}

type box struct {
	io.Reader
	size  int
	left  int
	name  string
	vers  byte
	flags [3]byte
}

func (b *box) readFull() ([]byte, error) {
	buf := make([]byte, b.size)
	if _, err := io.ReadFull(b, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (b *box) Read(p []byte) (n int, err error) {
	if b.left <= 0 {
		return 0, io.EOF
	}
	if len(p) > b.left {
		p = p[0:b.left]
	}
	n, err = b.Reader.Read(p)
	b.left -= n
	return
}

func readBox(r io.Reader) (b *box, err error) {
	b = new(box)
	header := make([]byte, 8)
	_, err = io.ReadFull(r, header)
	if err != nil {
		return
	}
	b.size = int(binary.BigEndian.Uint32(header[0:4])) - len(header)
	b.left = b.size
	b.name = string(header[4:8])
	b.Reader = r
	return
}

func readFullBox(r io.Reader) (b *box, err error) {
	b = new(box)
	header := make([]byte, 12)
	_, err = io.ReadFull(r, header)
	if err != nil {
		return
	}
	b.size = int(binary.BigEndian.Uint32(header[0:4])) - len(header)
	b.left = b.size
	b.name = string(header[4:8])
	b.vers = header[8]
	b.flags = [3]byte{header[9], header[10], header[11]}
	b.Reader = r
	return
}
