package xmpp

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"strings"
)

const (
	gtalkHost = "talk.google.com"
	gtalkAddr = "talk.google.com:443"
	nsStream  = "http://etherx.jabber.org/streams"
	nsTLS     = "urn:ietf:params:xml:ns:xmpp-tls"
	nsSASL    = "urn:ietf:params:xml:ns:xmpp-sasl"
	nsBind    = "urn:ietf:params:xml:ns:xmpp-bind"
	nsClient  = "jabber:client"
	nsNotify  = "google:mail:notify"
)

var DefaultConfig = tls.Config{
	ServerName: gtalkHost,
}

type Client struct {
	conn         *tls.Conn // connection to server
	jid          string    // Jabber ID for our connection
	domain       string
	p            *xml.Decoder
	user         string
	password     string
	errorHandler func(e error)
	mailHandler  func()
	debug        bool
}

func New(user, password string) *Client {
	return &Client{
		user:     user,
		password: password,
		errorHandler: func(e error) {
			fmt.Println(e)
		},
		mailHandler: func() {
			fmt.Println("NEW MAIL")
		},
	}
}

func (self *Client) Debug() *Client {
	self.debug = true
	return self
}

func (self *Client) MailHandler(f func()) *Client {
	self.mailHandler = f
	return self
}

func (self *Client) ErrorHandler(f func(e error)) *Client {
	self.errorHandler = f
	return self
}

func (self *Client) Start() (err error) {
	if err = self.connect(); err != nil {
		return
	}

	go self.handleMail()

	return
}

func (self *Client) handleMail() {
	for {
		name, i, err := next(self.p)
		if err != nil {
			if strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "reset") {
				self.Close()
				if e := self.Start(); e != nil {
					self.errorHandler(fmt.Errorf("While trying to restart after %v: %v", err, e))
				}
			} else {
				if self.errorHandler != nil {
					self.errorHandler(err)
				}
			}
			return
		}
		if name.Space == nsClient && name.Local == "iq" {
			if ciq, ok := i.(*clientIQ); ok && ciq.To == self.jid && ciq.Type == "set" && ciq.NewMail != nil {
				fmt.Fprintf(self.conn, "<iq type='result' from='%v' to='%v' id='%v' />\n", self.user, self.jid, ciq.Id)
				if self.mailHandler != nil {
					self.mailHandler()
				}
			}
		}
	}
}

func (self *Client) connect() (err error) {
	c, err := net.Dial("tcp", gtalkAddr)
	if err != nil {
		return
	}
	self.conn = tls.Client(c, &DefaultConfig)
	if err = self.conn.Handshake(); err != nil {
		return
	}
	if err = self.init(); err != nil {
		self.Close()
		return
	}

	return
}

func (self *Client) init() error {
	var r io.Reader
	r = self.conn
	if self.debug {
		r = tee{self.conn, os.Stdout}
	}

	self.p = xml.NewDecoder(r)

	a := strings.SplitN(self.user, "@", 2)
	if len(a) != 2 {
		return errors.New("xmpp: invalid username (want user@domain): " + self.user)
	}
	user := a[0]
	domain := a[1]

	// Declare intent to be a jabber client.
	fmt.Fprintf(self.conn, "<?xml version='1.0'?>\n"+
		"<stream:stream to='%s' xmlns='%s'\n"+
		" xmlns:stream='%s' version='1.0'>\n",
		xmlEscape(domain), nsClient, nsStream)

	// Server should respond with a stream opening.
	se, err := nextStart(self.p)
	if err != nil {
		return err
	}
	if se.Name.Space != nsStream || se.Name.Local != "stream" {
		return errors.New("xmpp: expected <stream> but got <" + se.Name.Local + "> in " + se.Name.Space)
	}

	// Now we're in the stream and can use Unmarshal.
	// Next message should be <features> to tell us authentication options.
	// See section 4.6 in RFC 3920.
	var f streamFeatures
	if err = self.p.DecodeElement(&f, nil); err != nil {
		return errors.New("unmarshal <features>: " + err.Error())
	}
	mechanism := ""
	for _, m := range f.Mechanisms.Mechanism {
		if m == "PLAIN" {
			mechanism = m
			// Plain authentication: send base64-encoded \x00 user \x00 password.
			raw := "\x00" + user + "\x00" + self.password
			enc := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
			base64.StdEncoding.Encode(enc, []byte(raw))
			fmt.Fprintf(self.conn, "<auth xmlns='%s' mechanism='PLAIN'>%s</auth>\n",
				nsSASL, enc)
			break
		}
		if m == "DIGEST-MD5" {
			mechanism = m
			// Digest-MD5 authentication
			fmt.Fprintf(self.conn, "<auth xmlns='%s' mechanism='DIGEST-MD5'/>\n",
				nsSASL)
			var ch saslChallenge
			if err = self.p.DecodeElement(&ch, nil); err != nil {
				return errors.New("unmarshal <challenge>: " + err.Error())
			}
			b, err := base64.StdEncoding.DecodeString(string(ch))
			if err != nil {
				return err
			}
			tokens := map[string]string{}
			for _, token := range strings.Split(string(b), ",") {
				kv := strings.SplitN(strings.TrimSpace(token), "=", 2)
				if len(kv) == 2 {
					if kv[1][0] == '"' && kv[1][len(kv[1])-1] == '"' {
						kv[1] = kv[1][1 : len(kv[1])-1]
					}
					tokens[kv[0]] = kv[1]
				}
			}
			realm, _ := tokens["realm"]
			nonce, _ := tokens["nonce"]
			qop, _ := tokens["qop"]
			charset, _ := tokens["charset"]
			cnonceStr := cnonce()
			digestUri := "xmpp/" + domain
			nonceCount := fmt.Sprintf("%08x", 1)
			digest := saslDigestResponse(user, realm, self.password, nonce, cnonceStr, "AUTHENTICATE", digestUri, nonceCount)
			message := "username=" + user + ", realm=" + realm + ", nonce=" + nonce + ", cnonce=" + cnonceStr + ", nc=" + nonceCount + ", qop=" + qop + ", digest-uri=" + digestUri + ", response=" + digest + ", charset=" + charset
			fmt.Fprintf(self.conn, "<response xmlns='%s'>%s</response>\n", nsSASL, base64.StdEncoding.EncodeToString([]byte(message)))

			var rspauth saslRspAuth
			if err = self.p.DecodeElement(&rspauth, nil); err != nil {
				return errors.New("unmarshal <challenge>: " + err.Error())
			}
			b, err = base64.StdEncoding.DecodeString(string(rspauth))
			if err != nil {
				return err
			}
			fmt.Fprintf(self.conn, "<response xmlns='%s'/>\n", nsSASL)
			break
		}
	}
	if mechanism == "" {
		return errors.New(fmt.Sprintf("PLAIN authentication is not an option: %v", f.Mechanisms.Mechanism))
	}

	// Next message should be either success or failure.
	name, val, err := next(self.p)
	if err != nil {
		return err
	}
	switch v := val.(type) {
	case *saslSuccess:
	case *saslFailure:
		// v.Any is type of sub-element in failure,
		// which gives a description of what failed.
		return errors.New("auth failure: " + v.Any.Local)
	default:
		return errors.New("expected <success> or <failure>, got <" + name.Local + "> in " + name.Space)
	}

	// Now that we're authenticated, we're supposed to start the stream over again.
	// Declare intent to be a jabber client.
	fmt.Fprintf(self.conn, "<stream:stream to='%s' xmlns='%s'\n"+
		" xmlns:stream='%s' version='1.0'>\n",
		xmlEscape(domain), nsClient, nsStream)

	// Here comes another <stream> and <features>.
	se, err = nextStart(self.p)
	if err != nil {
		return err
	}
	if se.Name.Space != nsStream || se.Name.Local != "stream" {
		return errors.New("expected <stream>, got <" + se.Name.Local + "> in " + se.Name.Space)
	}
	if err = self.p.DecodeElement(&f, nil); err != nil {
		return errors.New("unmarshal <features>: " + err.Error())
	}

	fmt.Fprintf(self.conn, "<iq type='set' id='x'><bind xmlns='%s'></bind></iq>\n", nsBind)
	var iq clientIQ
	if err = self.p.DecodeElement(&iq, nil); err != nil {
		return errors.New("unmarshal <iq>: " + err.Error())
	}
	if &iq.Bind == nil {
		return errors.New("<iq> result missing <bind>")
	}
	self.jid = iq.Bind.Jid // our local id

	// Make sure we have enabled the notifications
	fmt.Fprintf(self.conn, "<iq type='set' id='setting-1'><usersetting xmlns='google:setting'><mailnotifications value='true'/></usersetting></iq>")

	// Check the incoming iq
	name, i, err := next(self.p)
	if err != nil {
		return err
	}
	if name.Space != nsClient || name.Local != "iq" {
		return errors.New("expected <iq>, got <" + name.Local + "> in " + name.Space)
	}
	if iq, ok := i.(*clientIQ); !ok {
		return errors.New(fmt.Sprintf("expected <iq> got %v", i))
	} else if iq.To != self.jid || iq.Type != "result" {
		return errors.New(fmt.Sprintf("expected <iq> to %v with type 'result', got %v", self.jid, iq))
	}

	fmt.Fprintf(self.conn, "<iq type='get' to='%s'><query xmlns='http://jabber.org/protocol/disco#info'/></iq>", domain)

	name, i, err = next(self.p)
	if name.Space != nsClient || name.Local != "iq" {
		return errors.New("expected <iq>, got <" + name.Local + "> in " + name.Space)
	}
	ciq, ok := i.(*clientIQ)
	if !ok {
		return errors.New(fmt.Sprintf("expected <iq> got %v", i))
	} else if ciq.From != domain || ciq.To != self.jid || ciq.Type != "result" {
		return errors.New(fmt.Sprintf("expected <iq> from %#v, to %#v of type 'result' but got %#v, %#v, %#v", domain, self.jid, ciq.From, ciq.To, ciq.Type))
	}

	found := false
	for _, feature := range ciq.Query.Features {
		if feature.Var == nsNotify {
			found = true
			break
		}
	}
	if !found {
		return errors.New(fmt.Sprintf("expected to find %v, but got %+v", nsNotify, ciq.Query.Features))
	}

	fmt.Fprintf(self.conn, fmt.Sprintf("<iq type='get' from='%v'	to='%v' id='mail-request-1'><query xmlns='google:mail:notify'/></iq>", self.jid, self.user))

	name, i, err = next(self.p)
	if name.Space != nsClient || name.Local != "iq" {
		return errors.New(fmt.Sprintf("expected <iq> got %v", i))
	}
	ciq, ok = i.(*clientIQ)
	if !ok {
		return errors.New(fmt.Sprintf("expected <iq> got %v", i))
	} else if ciq.From != self.user || ciq.Id != "mail-request-1" || ciq.To != self.jid || ciq.Type != "result" {
		return errors.New(fmt.Sprintf("expected <iq> from %#v to %#v of type 'result', with id 'mail-request-1', but got %v", self.user, self.jid, ciq))
	}

	return nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func saslDigestResponse(username, realm, passwd, nonce, cnonceStr,
	authenticate, digestUri, nonceCountStr string) string {
	h := func(text string) []byte {
		h := md5.New()
		h.Write([]byte(text))
		return h.Sum(nil)
	}
	hex := func(bytes []byte) string {
		return fmt.Sprintf("%x", bytes)
	}
	kd := func(secret, data string) []byte {
		return h(secret + ":" + data)
	}

	a1 := string(h(username+":"+realm+":"+passwd)) + ":" +
		nonce + ":" + cnonceStr
	a2 := authenticate + ":" + digestUri
	response := hex(kd(hex(h(a1)), nonce+":"+
		nonceCountStr+":"+cnonceStr+":auth:"+
		hex(h(a2))))
	return response
}

func cnonce() string {
	randSize := big.NewInt(0)
	randSize.Lsh(big.NewInt(1), 64)
	cn, err := rand.Int(rand.Reader, randSize)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%016x", cn)
}

// RFC 3920  C.1  Streams name space
type streamFeatures struct {
	XMLName    xml.Name `xml:"http://etherx.jabber.org/streams features"`
	StartTLS   tlsStartTLS
	Mechanisms saslMechanisms
	Bind       bindBind
	Session    bool
}

type streamError struct {
	XMLName xml.Name `xml:"http://etherx.jabber.org/streams error"`
	Any     xml.Name
	Text    string
}

// RFC 3920  C.3  TLS name space

type tlsStartTLS struct {
	XMLName  xml.Name `xml:":ietf:params:xml:ns:xmpp-tls starttls"`
	Required bool
}

type tlsProceed struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-tls proceed"`
}

type tlsFailure struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-tls failure"`
}

// RFC 3920  C.4  SASL name space

type saslMechanisms struct {
	XMLName   xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl mechanisms"`
	Mechanism []string `xml:"mechanism"`
}

type saslAuth struct {
	XMLName   xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl auth"`
	Mechanism string   `xml:",attr"`
}

type saslChallenge string

type saslRspAuth string

type saslResponse string

type saslAbort struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl abort"`
}

type saslSuccess struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl success"`
}

type saslFailure struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-sasl failure"`
	Any     xml.Name
}

// RFC 3920  C.5  Resource binding name space

type bindBind struct {
	XMLName  xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-bind bind"`
	Resource string
	Jid      string `xml:"jid"`
}

// RFC 3921  B.1  jabber:client

type clientMessage struct {
	XMLName xml.Name `xml:"jabber:client message"`
	From    string   `xml:"from,attr"`
	Id      string   `xml:"id,attr"`
	To      string   `xml:"to,attr"`
	Type    string   `xml:"type,attr"` // chat, error, groupchat, headline, or normal

	// These should technically be []clientText,
	// but string is much more convenient.
	Subject string `xml:"subject"`
	Body    string `xml:"body"`
	Thread  string `xml:"thread"`

	// Any hasn't matched element
	Other []string `xml:",any"`
}

type clientText struct {
	Lang string `xml:",attr"`
	Body string `xml:"chardata"`
}

type clientPresence struct {
	XMLName xml.Name `xml:"jabber:client presence"`
	From    string   `xml:"from,attr"`
	Id      string   `xml:"id,attr"`
	To      string   `xml:"to,attr"`
	Type    string   `xml:"type,attr"` // error, probe, subscribe, subscribed, unavailable, unsubscribe, unsubscribed
	Lang    string   `xml:"lang,attr"`

	Show     string `xml:"show"`        // away, chat, dnd, xa
	Status   string `xml:"status,attr"` // sb []clientText
	Priority string `xml:"priority,attr"`
	Error    *clientError
}

type clientIQ struct { // info/query
	XMLName xml.Name `xml:"jabber:client iq"`
	From    string   `xml:"from,attr"`
	Id      string   `xml:"id,attr"`
	To      string   `xml:"to,attr"`
	Type    string   `xml:"type,attr"` // error, get, result, set
	Error   clientError
	Bind    bindBind
	Query   query
	NewMail *newMail
}

type newMail struct {
	XMLName xml.Name `xml:"new-mail"`
}

type query struct {
	XMLName  xml.Name  `xml:"query"`
	Identity identity  `xml:"identity"`
	Features []feature `xml:"feature"`
}

type identity struct {
	XMLName  xml.Name `xml:"identity"`
	Category string   `xml:"category,attr"`
	Type     string   `xml:"type,attr"`
	Name     string   `xml:"name,attr"`
}

type feature struct {
	Var string `xml:"var,attr"`
}

type clientError struct {
	XMLName xml.Name `xml:"jabber:client error"`
	Code    string   `xml:",attr"`
	Type    string   `xml:",attr"`
	Any     xml.Name
	Text    string
}

// Scan XML token stream to find next StartElement.
func nextStart(p *xml.Decoder) (xml.StartElement, error) {
	for {
		t, err := p.Token()
		if err != nil && err != io.EOF {
			return xml.StartElement{}, err
		}
		switch t := t.(type) {
		case xml.StartElement:
			return t, nil
		}
	}
	log.Printf("github.com/zond/gmail/xmpp unable to find next start element!")
	panic("unreachable")
}

// Scan XML token stream for next element and save into val.
// If val == nil, allocate new element based on proto map.
// Either way, return val.
func next(p *xml.Decoder) (xml.Name, interface{}, error) {
	// Read start element to find out what type we want.
	se, err := nextStart(p)
	if err != nil {
		return xml.Name{}, nil, err
	}

	// Put it in an interface and allocate one.
	var nv interface{}
	switch se.Name.Space + " " + se.Name.Local {
	case nsStream + " features":
		nv = &streamFeatures{}
	case nsStream + " error":
		nv = &streamError{}
	case nsTLS + " starttls":
		nv = &tlsStartTLS{}
	case nsTLS + " proceed":
		nv = &tlsProceed{}
	case nsTLS + " failure":
		nv = &tlsFailure{}
	case nsSASL + " mechanisms":
		nv = &saslMechanisms{}
	case nsSASL + " challenge":
		nv = ""
	case nsSASL + " response":
		nv = ""
	case nsSASL + " abort":
		nv = &saslAbort{}
	case nsSASL + " success":
		nv = &saslSuccess{}
	case nsSASL + " failure":
		nv = &saslFailure{}
	case nsBind + " bind":
		nv = &bindBind{}
	case nsClient + " message":
		nv = &clientMessage{}
	case nsClient + " presence":
		nv = &clientPresence{}
	case nsClient + " iq":
		nv = &clientIQ{}
	case nsClient + " error":
		nv = &clientError{}
	default:
		return xml.Name{}, nil, errors.New("unexpected XMPP message " +
			se.Name.Space + " <" + se.Name.Local + "/>")
	}

	// Unmarshal into that storage.
	if err = p.DecodeElement(nv, &se); err != nil {
		return xml.Name{}, nil, err
	}
	return se.Name, nv, err
}

var xmlSpecial = map[byte]string{
	'<':  "&lt;",
	'>':  "&gt;",
	'"':  "&quot;",
	'\'': "&apos;",
	'&':  "&amp;",
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		if s, ok := xmlSpecial[c]; ok {
			b.WriteString(s)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

type tee struct {
	r io.Reader
	w io.Writer
}

func (t tee) Read(p []byte) (n int, err error) {
	n, err = t.r.Read(p)
	if n > 0 {
		t.w.Write(p[0:n])
		t.w.Write([]byte("\n"))
	}
	return
}
