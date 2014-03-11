package gmail

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"code.google.com/p/go-imap/go1/imap"
)

const (
	gtalkAddr = "talk.google.com:443"
	nsStream  = "http://etherx.jabber.org/streams"
	nsTLS     = "urn:ietf:params:xml:ns:xmpp-tls"
	nsSASL    = "urn:ietf:params:xml:ns:xmpp-sasl"
	nsBind    = "urn:ietf:params:xml:ns:xmpp-bind"
	nsClient  = "jabber:client"
	nsNotify  = "google:mail:notify"
)

type MailHandler func(i interface{})

var DefaultConfig tls.Config

type Client struct {
	conn   net.Conn // connection to server
	imapc  *imap.Client
	jid    string // Jabber ID for our connection
	domain string
	p      *xml.Decoder
	opts   *Options
}

func connect(host, user, passwd string) (net.Conn, error) {
	addr := host

	if strings.TrimSpace(host) == "" {
		a := strings.SplitN(user, "@", 2)
		if len(a) == 2 {
			host = a[1]
		}
	}
	a := strings.SplitN(host, ":", 2)
	if len(a) == 1 {
		host += ":5222"
	}
	proxy := os.Getenv("HTTP_PROXY")
	if proxy == "" {
		proxy = os.Getenv("http_proxy")
	}
	if proxy != "" {
		url, err := url.Parse(proxy)
		if err == nil {
			addr = url.Host
		}
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	if proxy != "" {
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\n", host)
		fmt.Fprintf(c, "Host: %s\r\n", host)
		fmt.Fprintf(c, "\r\n")
		br := bufio.NewReader(c)
		req, _ := http.NewRequest("CONNECT", host, nil)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			f := strings.SplitN(resp.Status, " ", 2)
			return nil, errors.New(f[1])
		}
	}
	return c, nil
}

// Options are used to specify additional options for new clients, such as a Resource.
type Options struct {
	// Host specifies what host to connect to, as either "hostname" or "hostname:port"
	// If host is not specified, the  DNS SRV should be used to find the host from the domainpart of the JID.
	// Default the port to 5222.
	Host string

	// User specifies what user to authenticate to the remote server.
	User string

	// Password supplies the password to use for authentication with the remote server.
	Password string

	// Resource specifies an XMPP client resource, like "bot", instead of accepting one
	// from the server.  Use "" to let the server generate one for your client.
	Resource string

	// Debug output
	Debug bool

	// Mail handler function
	MailHandler MailHandler
}

// NewClient establishes a new Client connection based on a set of Options.
func (o Options) NewClient() *Client {
	return &Client{opts: &o}
}

func (self *Client) Start() (err error) {
	host := self.opts.Host
	c, err := connect(host, self.opts.User, self.opts.Password)
	if err != nil {
		return
	}

	tlsconn := tls.Client(c, &DefaultConfig)
	if err = tlsconn.Handshake(); err != nil {
		return
	}
	if strings.LastIndex(self.opts.Host, ":") > 0 {
		host = host[:strings.LastIndex(self.opts.Host, ":")]
	}
	if err = tlsconn.VerifyHostname(host); err != nil {
		return
	}
	self.conn = tlsconn

	if err = self.init(self.opts); err != nil {
		self.Close()
		return
	}

	go self.handleMail()

	self.imapc, err = imap.DialTLS("imap.gmail.com:993", nil)
	if err != nil {
		return
	}
	if _, err = self.imapc.Login(self.opts.User, self.opts.Password); err != nil {
		return
	}
	if _, err = self.imapc.Select("INBOX", false); err != nil {
		return
	}

	if err = self.checkMail(); err != nil {
		return
	}

	return
}

func (self *Client) checkMail() (err error) {
	cmd, err := self.imapc.UIDSearch("UNSEEN")
	if err != nil {
		return
	}
	fetchSeq := &imap.SeqSet{}
	for cmd.InProgress() {
		// Wait for the next response (no timeout)
		self.imapc.Recv(-1)

		// Process command data
		for _, rsp := range cmd.Data {
			for _, field := range rsp.Fields[1:] {
				fetchSeq.AddNum(field.(uint32))
			}
		}
		cmd.Data = nil
		self.imapc.Data = nil
	}

	var fetchCmd *imap.Command
	fetchCmd, err = self.imapc.UIDFetch(fetchSeq)
	if err != nil {
		return
	}
	for fetchCmd.InProgress() {
		// Wait for the next response (no timeout)
		self.imapc.Recv(-1)

		// Process command data
		for _, rsp := range fetchCmd.Data {
			fmt.Printf("%#v\n", rsp)
		}
		cmd.Data = nil
		self.imapc.Data = nil
	}

	return
}

func (self *Client) handleMail() {
	for {
		name, i, err := next(self.p)
		if err != nil {
			fmt.Println(err)
			self.Close()
			self.Start()
			return
		}
		if name.Space == nsClient && name.Local == "iq" {
			if ciq, ok := i.(*clientIQ); ok && ciq.To == self.jid && ciq.Type == "set" && ciq.NewMail != nil {
				fmt.Fprintf(self.conn, "<iq type='result' from='%v' to='%v' id='%v' />\n", self.opts.User, self.jid, ciq.Id)
				fmt.Println("NEW MAIL!")
			}
		}
	}
}

// NewClient creates a new connection to a host given as "hostname" or "hostname:port".
// If host is not specified, the  DNS SRV should be used to find the host from the domainpart of the JID.
// Default the port to 5222.
func NewClient(user, passwd string, mailHandler MailHandler) *Client {
	opts := Options{
		Host:        gtalkAddr,
		User:        user,
		Password:    passwd,
		Debug:       true,
		MailHandler: mailHandler,
	}
	return opts.NewClient()
}

func (c *Client) Close() error {
	err1 := c.conn.Close()
	_, err2 := c.imapc.Close(false)
	if err1 != nil {
		return err1
	}
	return err2
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

func (c *Client) init(o *Options) error {
	c.p = xml.NewDecoder(c.conn)
	// For debugging: the following causes the plaintext of the connection to be duplicated to stdout.
	if o.Debug {
		c.p = xml.NewDecoder(tee{c.conn, os.Stdout})
	}

	a := strings.SplitN(o.User, "@", 2)
	if len(a) != 2 {
		return errors.New("xmpp: invalid username (want user@domain): " + o.User)
	}
	user := a[0]
	domain := a[1]

	// Declare intent to be a jabber client.
	fmt.Fprintf(c.conn, "<?xml version='1.0'?>\n"+
		"<stream:stream to='%s' xmlns='%s'\n"+
		" xmlns:stream='%s' version='1.0'>\n",
		xmlEscape(domain), nsClient, nsStream)

	// Server should respond with a stream opening.
	se, err := nextStart(c.p)
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
	if err = c.p.DecodeElement(&f, nil); err != nil {
		return errors.New("unmarshal <features>: " + err.Error())
	}
	mechanism := ""
	for _, m := range f.Mechanisms.Mechanism {
		if m == "PLAIN" {
			mechanism = m
			// Plain authentication: send base64-encoded \x00 user \x00 password.
			raw := "\x00" + user + "\x00" + o.Password
			enc := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
			base64.StdEncoding.Encode(enc, []byte(raw))
			fmt.Fprintf(c.conn, "<auth xmlns='%s' mechanism='PLAIN'>%s</auth>\n",
				nsSASL, enc)
			break
		}
		if m == "DIGEST-MD5" {
			mechanism = m
			// Digest-MD5 authentication
			fmt.Fprintf(c.conn, "<auth xmlns='%s' mechanism='DIGEST-MD5'/>\n",
				nsSASL)
			var ch saslChallenge
			if err = c.p.DecodeElement(&ch, nil); err != nil {
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
			digest := saslDigestResponse(user, realm, o.Password, nonce, cnonceStr, "AUTHENTICATE", digestUri, nonceCount)
			message := "username=" + user + ", realm=" + realm + ", nonce=" + nonce + ", cnonce=" + cnonceStr + ", nc=" + nonceCount + ", qop=" + qop + ", digest-uri=" + digestUri + ", response=" + digest + ", charset=" + charset
			fmt.Fprintf(c.conn, "<response xmlns='%s'>%s</response>\n", nsSASL, base64.StdEncoding.EncodeToString([]byte(message)))

			var rspauth saslRspAuth
			if err = c.p.DecodeElement(&rspauth, nil); err != nil {
				return errors.New("unmarshal <challenge>: " + err.Error())
			}
			b, err = base64.StdEncoding.DecodeString(string(rspauth))
			if err != nil {
				return err
			}
			fmt.Fprintf(c.conn, "<response xmlns='%s'/>\n", nsSASL)
			break
		}
	}
	if mechanism == "" {
		return errors.New(fmt.Sprintf("PLAIN authentication is not an option: %v", f.Mechanisms.Mechanism))
	}

	// Next message should be either success or failure.
	name, val, err := next(c.p)
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
	fmt.Fprintf(c.conn, "<stream:stream to='%s' xmlns='%s'\n"+
		" xmlns:stream='%s' version='1.0'>\n",
		xmlEscape(domain), nsClient, nsStream)

	// Here comes another <stream> and <features>.
	se, err = nextStart(c.p)
	if err != nil {
		return err
	}
	if se.Name.Space != nsStream || se.Name.Local != "stream" {
		return errors.New("expected <stream>, got <" + se.Name.Local + "> in " + se.Name.Space)
	}
	if err = c.p.DecodeElement(&f, nil); err != nil {
		return errors.New("unmarshal <features>: " + err.Error())
	}

	// Send IQ message asking to bind to the local user name.
	if o.Resource == "" {
		fmt.Fprintf(c.conn, "<iq type='set' id='x'><bind xmlns='%s'></bind></iq>\n", nsBind)
	} else {
		fmt.Fprintf(c.conn, "<iq type='set' id='x'><bind xmlns='%s'><resource>%s</resource></bind></iq>\n", nsBind, o.Resource)
	}
	var iq clientIQ
	if err = c.p.DecodeElement(&iq, nil); err != nil {
		return errors.New("unmarshal <iq>: " + err.Error())
	}
	if &iq.Bind == nil {
		return errors.New("<iq> result missing <bind>")
	}
	c.jid = iq.Bind.Jid // our local id

	// Make sure we have enabled the notifications
	fmt.Fprintf(c.conn, "<iq type='set' id='setting-1'><usersetting xmlns='google:setting'><mailnotifications value='true'/></usersetting></iq>")

	// Check the incoming iq
	name, i, err := next(c.p)
	if err != nil {
		return err
	}
	if name.Space != nsClient || name.Local != "iq" {
		return errors.New("expected <iq>, got <" + name.Local + "> in " + name.Space)
	}
	if iq, ok := i.(*clientIQ); !ok {
		return errors.New(fmt.Sprintf("expected <iq> got %v", i))
	} else if iq.To != c.jid || iq.Type != "result" {
		return errors.New(fmt.Sprintf("expected <iq> to %v with type 'result', got %v", c.jid, iq))
	}

	fmt.Fprintf(c.conn, "<iq type='get' to='%s'><query xmlns='http://jabber.org/protocol/disco#info'/></iq>", domain)

	name, i, err = next(c.p)
	if name.Space != nsClient || name.Local != "iq" {
		return errors.New("expected <iq>, got <" + name.Local + "> in " + name.Space)
	}
	ciq, ok := i.(*clientIQ)
	if !ok {
		return errors.New(fmt.Sprintf("expected <iq> got %v", i))
	} else if ciq.From != domain || ciq.To != c.jid || ciq.Type != "result" {
		return errors.New(fmt.Sprintf("expected <iq> from %#v, to %#v of type 'result' but got %#v, %#v, %#v", domain, c.jid, ciq.From, ciq.To, ciq.Type))
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

	fmt.Fprintf(c.conn, fmt.Sprintf("<iq type='get' from='%v'	to='%v' id='mail-request-1'><query xmlns='google:mail:notify'/></iq>", c.jid, o.User))

	name, i, err = next(c.p)
	if name.Space != nsClient || name.Local != "iq" {
		return errors.New(fmt.Sprintf("expected <iq> got %v", i))
	}
	ciq, ok = i.(*clientIQ)
	if !ok {
		return errors.New(fmt.Sprintf("expected <iq> got %v", i))
	} else if ciq.From != o.User || ciq.Id != "mail-request-1" || ciq.To != c.jid || ciq.Type != "result" {
		return errors.New(fmt.Sprintf("expected <iq> from %#v to %#v of type 'result', with id 'mail-request-1', but got %v", o.User, c.jid, ciq))
	}

	return nil
}

type Chat struct {
	Remote string
	Type   string
	Text   string
	Other  []string
}

type Presence struct {
	From string
	To   string
	Type string
	Show string
}

// Recv wait next token of chat.
func (c *Client) Recv() (event interface{}, err error) {
	for {
		_, val, err := next(c.p)
		if err != nil {
			return Chat{}, err
		}
		switch v := val.(type) {
		case *clientMessage:
			return Chat{v.From, v.Type, v.Body, v.Other}, nil
		case *clientPresence:
			return Presence{v.From, v.To, v.Type, v.Show}, nil
		}
	}
	panic("unreachable")
}

// Send sends message text.
func (c *Client) Send(chat Chat) {
	fmt.Fprintf(c.conn, "<message to='%s' type='%s' xml:lang='en'>"+
		"<body>%s</body></message>",
		xmlEscape(chat.Remote), xmlEscape(chat.Type), xmlEscape(chat.Text))
}

// Send origin
func (c *Client) SendOrg(org string) {
	fmt.Fprint(c.conn, org)
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
