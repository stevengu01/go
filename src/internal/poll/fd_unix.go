// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin dragonfly freebsd linux nacl netbsd openbsd solaris

package poll

import (
	"io"
	"syscall"
)

// FD is a file descriptor. The net and os packages use this type as a
// field of a larger type representing a network connection or OS file.
type FD struct {
	// Lock sysfd and serialize access to Read and Write methods.
	fdmu fdMutex

	// System file descriptor. Immutable until Close.
	Sysfd int

	// I/O poller.
	pd pollDesc

	// Writev cache.
	iovecs *[]syscall.Iovec

	// Whether this is a streaming descriptor, as opposed to a
	// packet-based descriptor like a UDP socket. Immutable.
	IsStream bool

	// Whether a zero byte read indicates EOF. This is false for a
	// message based socket connection.
	ZeroReadIsEOF bool
}

// Init initializes the FD. The Sysfd field should already be set.
// This can be called multiple times on a single FD.
func (fd *FD) Init() error {
	return fd.pd.init(fd)
}

// Destroy closes the file descriptor. This is called when there are
// no remaining references.
func (fd *FD) destroy() error {
	// Poller may want to unregister fd in readiness notification mechanism,
	// so this must be executed before CloseFunc.
	fd.pd.close()
	err := CloseFunc(fd.Sysfd)
	fd.Sysfd = -1
	return err
}

// Close closes the FD. The underlying file descriptor is closed by the
// destroy method when there are no remaining references.
func (fd *FD) Close() error {
	if !fd.fdmu.increfAndClose() {
		return ErrClosing
	}
	// Unblock any I/O.  Once it all unblocks and returns,
	// so that it cannot be referring to fd.sysfd anymore,
	// the final decref will close fd.sysfd. This should happen
	// fairly quickly, since all the I/O is non-blocking, and any
	// attempts to block in the pollDesc will return ErrClosing.
	fd.pd.evict()
	// The call to decref will call destroy if there are no other
	// references.
	return fd.decref()
}

// Shutdown wraps the shutdown network call.
func (fd *FD) Shutdown(how int) error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	return syscall.Shutdown(fd.Sysfd, how)
}

// Darwin and FreeBSD can't read or write 2GB+ files at a time,
// even on 64-bit systems.
// The same is true of socket implementations on many systems.
// See golang.org/issue/7812 and golang.org/issue/16266.
// Use 1GB instead of, say, 2GB-1, to keep subsequent reads aligned.
const maxRW = 1 << 30

// Read implements io.Reader.
func (fd *FD) Read(p []byte) (int, error) {
	if err := fd.readLock(); err != nil {
		return 0, err
	}
	defer fd.readUnlock()
	if len(p) == 0 {
		// If the caller wanted a zero byte read, return immediately
		// without trying (but after acquiring the readLock).
		// Otherwise syscall.Read returns 0, nil which looks like
		// io.EOF.
		// TODO(bradfitz): make it wait for readability? (Issue 15735)
		return 0, nil
	}
	if err := fd.pd.prepareRead(); err != nil {
		return 0, err
	}
	if fd.IsStream && len(p) > maxRW {
		p = p[:maxRW]
	}
	for {
		n, err := syscall.Read(fd.Sysfd, p)
		if err != nil {
			n = 0
			if err == syscall.EAGAIN {
				if err = fd.pd.waitRead(); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, err
	}
}

// Pread wraps the pread system call.
func (fd *FD) Pread(p []byte, off int64) (int, error) {
	// Call incref, not readLock, because since pread specifies the
	// offset it is independent from other reads.
	// Similarly, using the poller doesn't make sense for pread.
	if err := fd.incref(); err != nil {
		return 0, err
	}
	if fd.IsStream && len(p) > maxRW {
		p = p[:maxRW]
	}
	n, err := syscall.Pread(fd.Sysfd, p, off)
	if err != nil {
		n = 0
	}
	fd.decref()
	err = fd.eofError(n, err)
	return n, err
}

// ReadFrom wraps the recvfrom network call.
func (fd *FD) ReadFrom(p []byte) (int, syscall.Sockaddr, error) {
	if err := fd.readLock(); err != nil {
		return 0, nil, err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(); err != nil {
		return 0, nil, err
	}
	for {
		n, sa, err := syscall.Recvfrom(fd.Sysfd, p, 0)
		if err != nil {
			n = 0
			if err == syscall.EAGAIN {
				if err = fd.pd.waitRead(); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, sa, err
	}
}

// ReadMsg wraps the recvmsg network call.
func (fd *FD) ReadMsg(p []byte, oob []byte) (int, int, int, syscall.Sockaddr, error) {
	if err := fd.readLock(); err != nil {
		return 0, 0, 0, nil, err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(); err != nil {
		return 0, 0, 0, nil, err
	}
	for {
		n, oobn, flags, sa, err := syscall.Recvmsg(fd.Sysfd, p, oob, 0)
		if err != nil {
			// TODO(dfc) should n and oobn be set to 0
			if err == syscall.EAGAIN {
				if err = fd.pd.waitRead(); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, oobn, flags, sa, err
	}
}

// Write implements io.Writer.
func (fd *FD) Write(p []byte) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(); err != nil {
		return 0, err
	}
	var nn int
	for {
		max := len(p)
		if fd.IsStream && max-nn > maxRW {
			max = nn + maxRW
		}
		n, err := syscall.Write(fd.Sysfd, p[nn:max])
		if n > 0 {
			nn += n
		}
		if nn == len(p) {
			return nn, err
		}
		if err == syscall.EAGAIN {
			if err = fd.pd.waitWrite(); err == nil {
				continue
			}
		}
		if err != nil {
			return nn, err
		}
		if n == 0 {
			return nn, io.ErrUnexpectedEOF
		}
	}
}

// Pwrite wraps the pwrite system call.
func (fd *FD) Pwrite(p []byte, off int64) (int, error) {
	// Call incref, not writeLock, because since pwrite specifies the
	// offset it is independent from other writes.
	// Similarly, using the poller doesn't make sense for pwrite.
	if err := fd.incref(); err != nil {
		return 0, err
	}
	defer fd.decref()
	var nn int
	for {
		max := len(p)
		if fd.IsStream && max-nn > maxRW {
			max = nn + maxRW
		}
		n, err := syscall.Pwrite(fd.Sysfd, p[nn:max], off+int64(nn))
		if n > 0 {
			nn += n
		}
		if nn == len(p) {
			return nn, err
		}
		if err != nil {
			return nn, err
		}
		if n == 0 {
			return nn, io.ErrUnexpectedEOF
		}
	}
}

// WriteTo wraps the sendto network call.
func (fd *FD) WriteTo(p []byte, sa syscall.Sockaddr) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(); err != nil {
		return 0, err
	}
	for {
		err := syscall.Sendto(fd.Sysfd, p, 0, sa)
		if err == syscall.EAGAIN {
			if err = fd.pd.waitWrite(); err == nil {
				continue
			}
		}
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
}

// WriteMsg wraps the sendmsg network call.
func (fd *FD) WriteMsg(p []byte, oob []byte, sa syscall.Sockaddr) (int, int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(); err != nil {
		return 0, 0, err
	}
	for {
		n, err := syscall.SendmsgN(fd.Sysfd, p, oob, sa, 0)
		if err == syscall.EAGAIN {
			if err = fd.pd.waitWrite(); err == nil {
				continue
			}
		}
		if err != nil {
			return n, 0, err
		}
		return n, len(oob), err
	}
}

// Accept wraps the accept network call.
func (fd *FD) Accept() (int, syscall.Sockaddr, string, error) {
	if err := fd.readLock(); err != nil {
		return -1, nil, "", err
	}
	defer fd.readUnlock()

	if err := fd.pd.prepareRead(); err != nil {
		return -1, nil, "", err
	}
	for {
		s, rsa, errcall, err := accept(fd.Sysfd)
		if err == nil {
			return s, rsa, "", err
		}
		switch err {
		case syscall.EAGAIN:
			if err = fd.pd.waitRead(); err == nil {
				continue
			}
		case syscall.ECONNABORTED:
			// This means that a socket on the listen
			// queue was closed before we Accept()ed it;
			// it's a silly error, so try again.
			continue
		}
		return -1, nil, errcall, err
	}
}

// Seek wraps syscall.Seek.
func (fd *FD) Seek(offset int64, whence int) (int64, error) {
	if err := fd.incref(); err != nil {
		return 0, err
	}
	defer fd.decref()
	return syscall.Seek(fd.Sysfd, offset, whence)
}

// ReadDirent wraps syscall.ReadDirent.
// We treat this like an ordinary system call rather than a call
// that tries to fill the buffer.
func (fd *FD) ReadDirent(buf []byte) (int, error) {
	if err := fd.incref(); err != nil {
		return 0, err
	}
	defer fd.decref()
	for {
		n, err := syscall.ReadDirent(fd.Sysfd, buf)
		if err != nil {
			n = 0
			if err == syscall.EAGAIN {
				if err = fd.pd.waitRead(); err == nil {
					continue
				}
			}
		}
		// Do not call eofError; caller does not expect to see io.EOF.
		return n, err
	}
}

// Fchdir wraps syscall.Fchdir.
func (fd *FD) Fchdir() error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	return syscall.Fchdir(fd.Sysfd)
}

// Fstat wraps syscall.Fstat
func (fd *FD) Fstat(s *syscall.Stat_t) error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	return syscall.Fstat(fd.Sysfd, s)
}

// On Unix variants only, expose the IO event for the net code.

// WaitWrite waits until data can be read from fd.
func (fd *FD) WaitWrite() error {
	return fd.pd.waitWrite()
}
