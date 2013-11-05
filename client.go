// Package sftp implements the SSH File Transfer Protocol as described in
// https://filezilla-project.org/specs/draft-ietf-secsh-filexfer-02.txt
package sftp

import (
	"io"
	"os"
	"path/filepath"
	"sync"

	"code.google.com/p/go.crypto/ssh"
)

// New creates a new sftp client on conn.
func NewClient(conn *ssh.ClientConn) (*Client, error) {
	s, err := conn.NewSession()
	if err != nil {
		return nil, err
	}
	if err := s.RequestSubsystem("sftp"); err != nil {
		return nil, err
	}
	pw, err := s.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, err := s.StdoutPipe()
	if err != nil {
		return nil, err
	}
	sftp := &Client{
		w: pw,
		r: pr,
	}
	if err := sftp.sendInit(); err != nil {
		return nil, err
	}
	return sftp, sftp.recvVersion()
}

type Client struct {
	w      io.WriteCloser
	r      io.Reader
	mu     sync.Mutex // locks mu and seralises commands to the server
	nextid uint32
}

func (c *Client) Close() error { return c.w.Close() }

func (c *Client) sendInit() error {
	type packet struct {
		Type       byte
		Version    uint32
		Extensions []struct {
			Name, Data string
		}
	}
	return sendPacket(c.w, packet{
		Type:    SSH_FXP_INIT,
		Version: 3, // http://tools.ietf.org/html/draft-ietf-secsh-filexfer-02
	})
}

// returns the current value of c.nextid and increments it
// callers is expected to hold c.mu
func (c *Client) nextId() uint32 {
	v := c.nextid
	c.nextid++
	return v
}

func (c *Client) recvVersion() error {
	typ, _, err := recvPacket(c.r)
	if err != nil {
		return err
	}
	if typ != SSH_FXP_VERSION {
		return &unexpectedPacketErr{SSH_FXP_VERSION, typ}
	}
	return nil
}

type Walker struct {
	c       *Client
	cur     item
	stack   []item
	descend bool
}

// Path returns the path to the most recent file or directory
// visited by a call to Step. It contains the argument to Walk
// as a prefix; that is, if Walk is called with "dir", which is
// a directory containing the file "a", Path will return "dir/a".
func (w *Walker) Path() string {
	return w.cur.path
}

// Stat returns info for the most recent file or directory
// visited by a call to Step.
func (w *Walker) Stat() os.FileInfo {
	return w.cur.info
}

// Err returns the error, if any, for the most recent attempt
// by Step to visit a file or directory. If a directory has
// an error, w will not descend into that directory.
func (w *Walker) Err() error {
	return w.cur.err
}

// SkipDir causes the currently visited directory to be skipped.
// If w is not on a directory, SkipDir has no effect.
func (w *Walker) SkipDir() {
	w.descend = false
}

type item struct {
	path string
	info os.FileInfo
	err  error
}

// Walk returns a new Walker rooted at root.
func (c *Client) Walk(root string) *Walker {
	info, err := c.Lstat(root)
	return &Walker{c: c, stack: []item{{root, info, err}}}
}

// Step advances the Walker to the next file or directory,
// which will then be available through the Path, Stat,
// and Err methods.
// It returns false when the walk stops at the end of the tree.
func (w *Walker) Step() bool {
	if w.descend && w.cur.err == nil && w.cur.info.IsDir() {
		list, err := w.c.readDir(w.cur.path)
		if err != nil {
			w.cur.err = err
			w.stack = append(w.stack, w.cur)
		} else {
			for i := len(list) - 1; i >= 0; i-- {
				path := filepath.Join(w.cur.path, list[i].Name())
				w.stack = append(w.stack, item{path, list[i], nil})
			}
		}
	}

	if len(w.stack) == 0 {
		return false
	}
	i := len(w.stack) - 1
	w.cur = w.stack[i]
	w.stack = w.stack[:i]
	w.descend = true
	return true
}

func (c *Client) readDir(path string) ([]os.FileInfo, error) {
	handle, err := c.opendir(path)
	if err != nil {
		return nil, err
	}
	var attrs []os.FileInfo
	c.mu.Lock()
	defer c.mu.Unlock()
	var done = false
	for !done {
		type packet struct {
			Type   byte
			Id     uint32
			Handle string
		}
		id := c.nextId()
		if err := sendPacket(c.w, packet{
			Type:   SSH_FXP_READDIR,
			Id:     id,
			Handle: handle,
		}); err != nil {
			return nil, err
		}
		typ, data, err := recvPacket(c.r)
		if err != nil {
			return nil, err
		}
		switch typ {
		case SSH_FXP_NAME:
			sid, data := unmarshalUint32(data)
			if sid != id {
				return nil, &unexpectedIdErr{id, sid}
			}
			count, data := unmarshalUint32(data)
			for i := uint32(0); i < count; i++ {
				var filename string
				filename, data = unmarshalString(data)
				_, data = unmarshalString(data) // discard longname
				var attr *attr
				attr, data = unmarshalAttrs(data)
				if filename == "." || filename == ".." {
					continue
				}
				attr.name = filename
				attrs = append(attrs, attr)
			}
		case SSH_FXP_STATUS:
			sid, data := unmarshalUint32(data)
			if sid != id {
				return nil, &unexpectedIdErr{id, sid}
			}
			code, data := unmarshalUint32(data)
			msg, data := unmarshalString(data)
			lang, _ := unmarshalString(data)
			err = &StatusError{
				Code: code,
				msg:  msg,
				lang: lang,
			}
			done = true
		default:
			return nil, unimplementedPacketErr(typ)
		}
	}
	// TODO(dfc) closedir
	return attrs, err
}
func (c *Client) opendir(path string) (string, error) {
	type packet struct {
		Type byte
		Id   uint32
		Path string
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextId()
	if err := sendPacket(c.w, packet{
		Type: SSH_FXP_OPENDIR,
		Id:   id,
		Path: path,
	}); err != nil {
		return "", err
	}
	typ, data, err := recvPacket(c.r)
	if err != nil {
		return "", err
	}
	switch typ {
	case SSH_FXP_HANDLE:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return "", &unexpectedIdErr{id, sid}
		}
		handle, _ := unmarshalString(data)
		return handle, nil
	case SSH_FXP_STATUS:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return "", &unexpectedIdErr{id, sid}
		}
		code, data := unmarshalUint32(data)
		msg, data := unmarshalString(data)
		lang, _ := unmarshalString(data)
		return "", &StatusError{
			Code: code,
			msg:  msg,
			lang: lang,
		}
	default:
		return "", unimplementedPacketErr(typ)
	}
}

func (c *Client) Lstat(path string) (os.FileInfo, error) {
	type packet struct {
		Type byte
		Id   uint32
		Path string
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextId()
	if err := sendPacket(c.w, packet{
		Type: SSH_FXP_LSTAT,
		Id:   id,
		Path: path,
	}); err != nil {
		return nil, err
	}
	typ, data, err := recvPacket(c.r)
	if err != nil {
		return nil, err
	}
	switch typ {
	case SSH_FXP_ATTRS:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return nil, &unexpectedIdErr{id, sid}
		}
		attr, _ := unmarshalAttrs(data)
		attr.name = path
		return attr, nil
	case SSH_FXP_STATUS:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return nil, &unexpectedIdErr{id, sid}
		}
		code, data := unmarshalUint32(data)
		msg, data := unmarshalString(data)
		lang, _ := unmarshalString(data)
		return nil, &StatusError{
			Code: code,
			msg:  msg,
			lang: lang,
		}
	default:
		return nil, unimplementedPacketErr(typ)
	}
}