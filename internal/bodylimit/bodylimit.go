package bodylimit

import (
	"errors"
	"fmt"
	"io"
)

var ErrTooLarge = errors.New("body exceeds configured limit")

func ReadPrefix(r io.Reader, limit int64) ([]byte, bool, error) {
	if limit < 0 {
		return nil, false, fmt.Errorf("invalid body limit %d", limit)
	}
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return buf, false, err
	}
	return buf, int64(len(buf)) > limit, nil
}

func ReadAll(r io.Reader, limit int64) ([]byte, error) {
	buf, overLimit, err := ReadPrefix(r, limit)
	if err != nil {
		return buf, err
	}
	if overLimit {
		return buf[:limit], ErrTooLarge
	}
	return buf, nil
}

func Error(label string, limit int64) error {
	return fmt.Errorf("%s exceeds %d bytes: %w", label, limit, ErrTooLarge)
}

func Wrap(label string, limit int64, err error) error {
	if errors.Is(err, ErrTooLarge) {
		return Error(label, limit)
	}
	return fmt.Errorf("read %s: %w", label, err)
}
