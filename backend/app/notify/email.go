package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"time"
	"text/template"

	log "github.com/go-pkgz/lgr"
	"github.com/go-pkgz/repeater"
	"github.com/pkg/errors"

	"github.com/umputun/remark/backend/app/templates"
)

// EmailParams contain settings for email notifications
type EmailParams struct {
	From                     string // from email address
	VerificationSubject      string // verification message sub
	VerificationTemplatePath string // path to verification message template
	ReplyTemplatePath        string // path to reply message template
	SubscribeURL             string // full subscribe handler URL
	UnsubscribeURL           string // full unsubscribe handler URL

	TokenGenFn func(userID, email, site string) (string, error) // Unsubscribe token generation function
}

// SmtpParams contain settings for smtp server connection
type SmtpParams struct {
	Host     string        // SMTP host
	Port     int           // SMTP port
	TLS      bool          // TLS auth
	Username string        // user name
	Password string        // password
	TimeOut  time.Duration // TCP connection timeout
}

// Email implements notify.Destination for email
type Email struct {
	EmailParams
	SmtpParams

	smtp smtpClientCreator
	replyTmpl *template.Template
	verificationTmpl *template.Template
}

// default email client implementation
type emailClient struct{ smtpClientCreator }

// smtpClient interface defines subset of net/smtp used by email client
type smtpClient interface {
	Mail(string) error
	Auth(smtp.Auth) error
	Rcpt(string) error
	Data() (io.WriteCloser, error)
	Quit() error
	Close() error
}

// smtpClientCreator interface defines function for creating new smtpClients
type smtpClientCreator interface {
	Create(SmtpParams) (smtpClient, error)
}

type emailMessage struct {
	from    string
	to      string
	message string
}

// replyTmplData store data for message from request template execution
type replyTmplData struct {
	UserName          string
	UserPicture       string
	CommentText       string
	CommentLink       string
	CommentDate       time.Time
	ParentUserName    string
	ParentUserPicture string
	ParentCommentText string
	ParentCommentLink string
	ParentCommentDate time.Time
	PostTitle         string
	Email             string
	UnsubscribeLink   string
	ForAdmin          bool
}

// verificationTmplData store data for verification message template execution
type verificationTmplData struct {
	User         string
	Token        string
	Email        string
	Site         string
	SubscribeURL string
}

const (
	defaultVerificationSubject = "Email verification"
	defaultEmailTimeout        = 10 * time.Second
	defaultEmailTemplatePath             = "../templates/email_reply.html.tmpl"
	defaultEmailVerificationTemplatePath = "../templates/email_confirmation.html.tmpl"
)

// NewEmail makes new Email object, returns error in case of e.ReplyTemplatePath or e.VerificationTemplatePath parsing error
func NewEmail(emailParams EmailParams, smtpParams SmtpParams) (*Email, error) {
	// set up Email emailParams
	res := Email{EmailParams: emailParams}
	res.smtp = &emailClient{}
	res.SmtpParams = smtpParams
	if res.TimeOut <= 0 {
		res.TimeOut = defaultEmailTimeout
	}

	if res.ReplyTemplatePath == "" {
		res.ReplyTemplatePath = defaultEmailTemplatePath
	}
	if res.VerificationTemplatePath == "" {
		res.VerificationTemplatePath = defaultEmailVerificationTemplatePath
	}
	if res.VerificationSubject == "" {
		res.VerificationSubject = defaultVerificationSubject
	}

	// initialise templates
	var err error
	var replyTmplFile, verificationTmplFile []byte
	fs := templates.Init()

	if replyTmplFile, err = fs.ReadFile(res.ReplyTemplatePath); err != nil {
		return nil, errors.Wrapf(err, "can't read reply template")
	}
	if verificationTmplFile, err = fs.ReadFile(res.VerificationTemplatePath); err != nil {
		return nil, errors.Wrapf(err, "can't read verification template")
	}
	if res.replyTmpl, err = template.New("replyTmpl").Parse(string(replyTmplFile)); err != nil {
		return nil, errors.Wrapf(err, "can't parse reply template")
	}
	if res.verificationTmpl, err = template.New("verificationTmpl").Parse(string(verificationTmplFile)); err != nil {
		return nil, errors.Wrapf(err, "can't parse verification template")
	}

	log.Printf("[DEBUG] Create new email notifier for server %s with user %s, timeout=%s",
		res.Host, res.Username, res.TimeOut)

	return &res, nil
}

// Send email about comment reply to Request.Email if it's set,
// also sends email to site administrator if appropriate option is set.
// Thread safe
func (e *Email) Send(ctx context.Context, req Request) (err error) {
	if req.Email == "" {
		// this means we can't send this request via Email
		return nil
	}
	select {
	case <-ctx.Done():
		return errors.Errorf("sending message to %q aborted due to canceled context", req.Email)
	default:
	}
	var msg string

	if req.Verification.Token != "" {
		log.Printf("[DEBUG] send verification via %s, user %s", e, req.Verification.User)
		msg, err = e.buildVerificationMessage(req.Verification.User, req.Email, req.Verification.Token, req.Verification.SiteID)
		if err != nil {
			return err
		}
	}

	if req.Comment.ID != "" {
		if req.parent.User.ID == req.Comment.User.ID && !req.ForAdmin {
			// don't send anything if if user replied to their own comment
			return nil
		}
		log.Printf("[DEBUG] send notification via %s, comment id %s", e, req.Comment.ID)
		msg, err = e.buildReplyMessage(req, req.ForAdmin)
		if err != nil {
			return err
		}
	}

	return repeater.NewDefault(5, time.Millisecond*250).Do(
		ctx,
		func() error {
			return e.sendMessage(emailMessage{from: e.From, to: req.Email, message: msg})
		})
}

// buildVerificationMessage generates verification email message based on given input
func (e *Email) buildVerificationMessage(user, email, token, site string) (string, error) {
	subject := e.VerificationSubject
	msg := bytes.Buffer{}
	err := e.verificationTmpl.Execute(&msg, verificationTmplData{
		User:         user,
		Token:        token,
		Email:        email,
		Site:         site,
		SubscribeURL: e.SubscribeURL,
	})
	if err != nil {
		return "", errors.Wrapf(err, "error executing template to build verification message")
	}
	return e.buildMessage(subject, msg.String(), email, "text/html", "")
}

// buildReplyMessage generates email message based on Request using e.replyTmpl
func (e *Email) buildReplyMessage(req Request, forAdmin bool) (string, error) {
	subject := "New reply to your comment"
	if forAdmin {
		subject = "New comment to your site"
	}
	if req.Comment.PostTitle != "" {
		subject += fmt.Sprintf(" for \"%s\"", req.Comment.PostTitle)
	}

	token, err := e.TokenGenFn(req.parent.User.ID, req.Email, req.Comment.Locator.SiteID)
	if err != nil {
		return "", errors.Wrapf(err, "error creating token for unsubscribe link")
	}
	unsubscribeLink := e.UnsubscribeURL + "?site=" + req.Comment.Locator.SiteID + "&tkn=" + token
	if forAdmin {
		unsubscribeLink = ""
	}

	commentUrlPrefix := req.Comment.Locator.URL + uiNav
	msg := bytes.Buffer{}
	tmplData := replyTmplData{
		UserName:        req.Comment.User.Name,
		UserPicture:     req.Comment.User.Picture,
		CommentText:     req.Comment.Text,
		CommentLink:     commentUrlPrefix + req.Comment.ID,
		CommentDate:     req.Comment.Timestamp,
		PostTitle:       req.Comment.PostTitle,
		Email:           req.Email,
		UnsubscribeLink: unsubscribeLink,
		ForAdmin:        forAdmin,
	}
	// in case of message to admin, parent message might be empty
	if req.Comment.ParentID != "" {
		tmplData.ParentUserName = req.parent.User.Name
		tmplData.ParentUserPicture = req.parent.User.Picture
		tmplData.ParentCommentText = req.parent.Text
		tmplData.ParentCommentLink = commentUrlPrefix + req.parent.ID
		tmplData.ParentCommentDate = req.parent.Timestamp
	}
	err = e.replyTmpl.Execute(&msg, tmplData)
	if err != nil {
		return "", errors.Wrapf(err, "error executing template to build comment reply message")
	}
	return e.buildMessage(subject, msg.String(), req.Email, "text/html", unsubscribeLink)
}

// buildMessage generates email message to send using net/smtp.Data()
func (e *Email) buildMessage(subject, body, to, contentType, unsubscribeLink string) (message string, err error) {
	addHeader := func(msg, h, v string) string {
		msg += fmt.Sprintf("%s: %s\n", h, v)
		return msg
	}
	message = addHeader(message, "From", e.From)
	message = addHeader(message, "To", to)
	message = addHeader(message, "Subject", subject)
	message = addHeader(message, "Content-Transfer-Encoding", "quoted-printable")

	if contentType != "" {
		message = addHeader(message, "MIME-version", "1.0")
		message = addHeader(message, "Content-Type", contentType+`; charset="UTF-8"`)
	}

	if unsubscribeLink != "" {
		// https://support.google.com/mail/answer/81126 -> "Include option to unsubscribe"
		message = addHeader(message, "List-Unsubscribe-Post", "List-Unsubscribe=One-Click")
		message = addHeader(message, "List-Unsubscribe", "<"+unsubscribeLink+">")
	}

	message = addHeader(message, "Date", time.Now().Format(time.RFC1123Z))

	buff := &bytes.Buffer{}
	qp := quotedprintable.NewWriter(buff)
	if _, err := qp.Write([]byte(body)); err != nil {
		return "", err
	}
	// flush now, must NOT use defer, for small body, defer may cause buff.String() got empty body
	if err := qp.Close(); err != nil {
		return "", fmt.Errorf("quotedprintable Write failed: %w", err)
	}
	m := buff.String()
	message += "\n" + m
	return message, nil
}

// sendMessage sends messages to server in a new connection, closing the connection after finishing.
// Thread safe.
func (e *Email) sendMessage(m emailMessage) error {
	if e.smtp == nil {
		return errors.New("sendMessage called without smtpClient set")
	}
	smtpClient, err := e.smtp.Create(e.SmtpParams)
	if err != nil {
		return errors.Wrap(err, "failed to make smtp Create")
	}

	defer func() {
		if err := smtpClient.Quit(); err != nil {
			log.Printf("[WARN] failed to send quit command to %s:%d, %v", e.Host, e.Port, err)
			if err := smtpClient.Close(); err != nil {
				log.Printf("[WARN] can't close smtp connection, %v", err)
			}
		}
	}()

	if err := smtpClient.Mail(m.from); err != nil {
		return errors.Wrapf(err, "bad from address %q", m.from)
	}
	if err := smtpClient.Rcpt(m.to); err != nil {
		return errors.Wrapf(err, "bad to address %q", m.to)
	}

	writer, err := smtpClient.Data()
	if err != nil {
		return errors.Wrap(err, "can't make email writer")
	}

	defer func() {
		if err = writer.Close(); err != nil {
			log.Printf("[WARN] can't close smtp body writer, %v", err)
		}
	}()

	buf := bytes.NewBufferString(m.message)
	if _, err = buf.WriteTo(writer); err != nil {
		return errors.Wrapf(err, "failed to send email body to %q", m.to)
	}

	return nil
}

// String representation of Email object
func (e *Email) String() string {
	return fmt.Sprintf("email: from %q with username '%s' at server %s:%d", e.From, e.Username, e.Host, e.Port)
}

// Create establish SMTP connection with server using credentials in smtpClientWithCreator.SmtpParams
// and returns pointer to it. Thread safe.
func (s *emailClient) Create(params SmtpParams) (smtpClient, error) {
	authenticate := func(c *smtp.Client) error {
		if params.Username == "" || params.Password == "" {
			return nil
		}
		auth := smtp.PlainAuth("", params.Username, params.Password, params.Host)
		if err := c.Auth(auth); err != nil {
			return errors.Wrapf(err, "failed to auth to smtp %s:%d", params.Host, params.Port)
		}
		return nil
	}

	var c *smtp.Client
	srvAddress := fmt.Sprintf("%s:%d", params.Host, params.Port)
	if params.TLS {
		tlsConf := &tls.Config{
			InsecureSkipVerify: false,
			ServerName:         params.Host,
		}
		conn, err := tls.Dial("tcp", srvAddress, tlsConf)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to dial smtp tls to %s", srvAddress)
		}
		if c, err = smtp.NewClient(conn, params.Host); err != nil {
			return nil, errors.Wrapf(err, "failed to make smtp client for %s", srvAddress)
		}
		return c, authenticate(c)
	}

	conn, err := net.DialTimeout("tcp", srvAddress, params.TimeOut)
	if err != nil {
		return nil, errors.Wrapf(err, "timeout connecting to %s", srvAddress)
	}

	c, err = smtp.NewClient(conn, params.Host)
	if err != nil {
		return nil, errors.Wrap(err, "failed to dial")
	}

	return c, authenticate(c)
}
