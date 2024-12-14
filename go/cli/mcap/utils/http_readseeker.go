package utils

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"syscall"
)

// Interval represents a range of downloaded bytes
type Interval struct {
	Start, End int64
}

type Option func(*HTTPSeeker)

func WithMinRequestSize(size int64) Option {
	return func(hs *HTTPSeeker) {
		hs.minRequestSize = size
	}
}

func WithHeaders(headers http.Header) Option {
	return func(hs *HTTPSeeker) {
		hs.headers = headers
	}
}

type HTTPSeeker struct {
	url            string
	size           int64
	pos            int64
	minRequestSize int64
	headers        http.Header
	buffer         []byte
	intervals      []Interval
}

func NewHTTPSeeker(url string, opts ...Option) (*HTTPSeeker, error) {
	hs := &HTTPSeeker{
		url:            url,
		minRequestSize: 32 * 1024, // Default 32KB minimum request size
		headers:        make(http.Header),
		intervals:      []Interval{},
	}

	for _, opt := range opts {
		opt(hs)
	}

	// Get file size
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hs.headers {
		req.Header[k] = v
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	hs.size, err = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return nil, err
	}

	// Create memory mapping
	hs.buffer, err = syscall.Mmap(
		-1,
		0,
		int(hs.size),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON,
	)
	if err != nil {
		return nil, err
	}

	return hs, nil
}

// start = first byte to download; end = one past the last byte to download
func (hs *HTTPSeeker) downloadRange(start int64, end int64) (downloaded_start int64, downloaded_end int64, err error) {
	fmt.Printf("\tdownloadRange(%d, %d)\n", start, end)
	if end-start < hs.minRequestSize {
		end = start + hs.minRequestSize
	}
	// FIXME: end = hs.size - 1?
	if end > hs.size {
		end = hs.size
	}
	if end-start < hs.minRequestSize {
		start = end - hs.minRequestSize
	}
	if start < 0 {
		start = 0
	}

	fmt.Printf("\t\tdownloadRange modified to %d, %d. EOF is %d\n", start, end, hs.size)
	req, err := http.NewRequest("GET", hs.url, nil)
	if err != nil {
		return 0, 0, err
	}

	// FIXME: end - 1? end + 1?
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	for k, v := range hs.headers {
		req.Header[k] = v
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		return 0, 0, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	n, err := io.ReadFull(resp.Body, hs.buffer[start:end])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, 0, err
	}
	if int64(n) != end-start {
		return 0, 0, fmt.Errorf("Expected to read %d bytes but got %d", end-start, n)
	}
	return start, end, nil
}

func (hs *HTTPSeeker) Read(b []byte) (n int, err error) {
	if hs.pos >= hs.size {
		return 0, io.EOF
	}

	requestedReadSize := int64(len(b))
	requestedEnd := hs.pos + requestedReadSize
	if requestedEnd > hs.size {
		requestedEnd = hs.size
	}
	fmt.Printf("Read request - offset %d, length %d, fileSize %d\n", hs.pos, requestedReadSize, hs.size)
	if hs.pos+requestedReadSize > hs.size {
		requestedReadSize = hs.size - hs.pos
		fmt.Printf("\tRequested truncated to EOF - %d bytes\n", requestedReadSize)
	}

	for {
		fmt.Printf("\tlooking for %d to %d intervals=%v\n", hs.pos, requestedEnd, hs.intervals)
		n, found := slices.BinarySearchFunc(hs.intervals, hs.pos, func(interval Interval, target int64) int {
			return int(interval.Start - target)
		})
		fmt.Printf("\tn=%d\n", n)

		// if found == true then n is the interval that starts where we want
		// otherwise, n
		if (found && hs.intervals[n].End >= requestedEnd) || (n > 0 && hs.intervals[n-1].End >= requestedEnd) {
			fmt.Printf("\tFound!\n")
			break
		}

		var err error
		downloadStart := hs.pos
		downloadEnd := requestedEnd

		// If there is a downloaded range following this request, plug the "gap" until its start
		if len(hs.intervals) > n+1 && hs.intervals[n+1].Start < requestedEnd {
			downloadEnd = hs.intervals[n+1].Start
		}

		// downloadRange may download a different range than requested
		downloadStart, downloadEnd, err = hs.downloadRange(downloadStart, downloadEnd)
		if err != nil {
			return 0, err
		}

		if len(hs.intervals) > n+1 && hs.intervals[n+1].Start < requestedEnd {
			hs.intervals[n+1].Start = downloadStart
		} else {
			newIntervals := make([]Interval, len(hs.intervals)+1)
			copy(newIntervals[:n], hs.intervals[:n])
			newIntervals[n] = Interval{downloadStart, downloadEnd}
			if n < len(hs.intervals) {
				copy(newIntervals[n+1:], hs.intervals[n+1:])
			}
			hs.intervals = newIntervals
		}
	}

	n = copy(b, hs.buffer[hs.pos:hs.pos+requestedReadSize])
	hs.pos += int64(n)
	return n, nil
}

func (hs *HTTPSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = hs.pos + offset
	case io.SeekEnd:
		abs = hs.size + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}

	if abs < 0 {
		return 0, fmt.Errorf("negative position")
	}

	if abs > hs.size {
		return 0, fmt.Errorf("seek beyond end of file")
	}

	hs.pos = abs
	return abs, nil
}

func (hs *HTTPSeeker) Close() error {
	return syscall.Munmap(hs.buffer)
}
