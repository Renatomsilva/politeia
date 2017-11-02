package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dajohi/goemail"
	pd "github.com/decred/politeia/politeiad/api/v1"
	"github.com/decred/politeia/politeiad/api/v1/mime"
	www "github.com/decred/politeia/politeiawww/api/v1"
	"github.com/decred/politeia/politeiawww/database"
	"github.com/decred/politeia/politeiawww/database/localdb"
	"github.com/decred/politeia/util"
	"github.com/kennygrant/sanitize"
)

// politeiawww backend construct
type backend struct {
	db        database.Database
	cfg       *config
	inventory []www.ProposalRecord

	// These properties are only used for testing.
	test                   bool
	verificationExpiryTime time.Duration
}

// userError represents an error that is caused by something that the user
// did (malformed input, bad timing, etc).
type userError struct {
	errorCode www.StatusT
}

// Error satisfies the error interface.
func (e userError) Error() string {
	return fmt.Sprintf("user error code: %v", e.errorCode)
}

func (b *backend) getVerificationExpiryTime() time.Duration {
	if b.verificationExpiryTime != time.Duration(0) {
		return b.verificationExpiryTime
	}
	return time.Duration(www.VerificationExpiryHours) * time.Hour
}

func (b *backend) generateVerificationTokenAndExpiry() ([]byte, int64, error) {
	token, err := util.Random(www.VerificationTokenSize)
	if err != nil {
		return nil, 0, err
	}

	expiry := time.Now().Add(b.getVerificationExpiryTime()).Unix()

	return token, expiry, nil
}

// emailNewUserVerificationLink emails the link with the new user verification token
// if the email server is set up.
func (b *backend) emailNewUserVerificationLink(email, token string) error {
	if b.cfg.SMTP == nil {
		return nil
	}

	l, err := url.Parse(b.cfg.WebServerAddress + www.RouteVerifyNewUser)
	if err != nil {
		return err
	}
	q := l.Query()
	q.Set("email", email)
	q.Set("verificationtoken", token)
	l.RawQuery = q.Encode()

	var buf bytes.Buffer
	tplData := newUserEmailTemplateData{
		Email: email,
		Link:  l.String(),
	}
	err = templateNewUserEmail.Execute(&buf, &tplData)
	if err != nil {
		return err
	}
	from := "noreply@decred.org"
	subject := "Politeia Registration - Verify Your Email"
	body := buf.String()

	msg := goemail.NewHTMLMessage(from, subject, body)
	msg.AddTo(email)

	return b.cfg.SMTP.Send(msg)
}

// emailResetPasswordVerificationLink emails the link with the reset password
// verification token if the email server is set up.
func (b *backend) emailResetPasswordVerificationLink(email, token string) error {
	if b.cfg.SMTP == nil {
		return nil
	}

	l, err := url.Parse(b.cfg.WebServerAddress + www.RouteResetPassword)
	if err != nil {
		return err
	}
	q := l.Query()
	q.Set("email", email)
	q.Set("verificationtoken", token)
	l.RawQuery = q.Encode()

	var buf bytes.Buffer
	tplData := resetPasswordEmailTemplateData{
		Email: email,
		Link:  l.String(),
	}
	err = templateResetPasswordEmail.Execute(&buf, &tplData)
	if err != nil {
		return err
	}
	from := "noreply@decred.org"
	subject := "Politeia - Reset Your Password"
	body := buf.String()

	msg := goemail.NewHTMLMessage(from, subject, body)
	msg.AddTo(email)

	return b.cfg.SMTP.Send(msg)
}

// makeRequest makes an http request to the method and route provided, serializing
// the provided object as the request body.
func (b *backend) makeRequest(method string, route string, v interface{}) ([]byte, error) {
	var requestBody []byte
	if v != nil {
		var err error
		requestBody, err = json.Marshal(v)
		if err != nil {
			return nil, err
		}
	}

	fullRoute := b.cfg.RPCHost + route

	c, err := util.NewClient(false, b.cfg.RPCCert)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, fullRoute, bytes.NewReader(requestBody))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(b.cfg.RPCUser, b.cfg.RPCPass)
	r, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()

	if r.StatusCode != http.StatusOK {
		e, err := util.GetErrorFromJSON(r.Body)
		if err != nil {
			return nil, fmt.Errorf("%v", r.Status)
		}
		return nil, fmt.Errorf("%v: %v", r.Status, e)
	}

	responseBody := util.ConvertBodyToByteArray(r.Body, false)
	return responseBody, nil
}

// remoteInventory fetches the entire inventory of proposals from politeiad.
func (b *backend) remoteInventory() (*pd.InventoryReply, error) {
	challenge, err := util.Random(pd.ChallengeSize)
	if err != nil {
		return nil, err
	}
	inv := pd.Inventory{
		Challenge:     hex.EncodeToString(challenge),
		IncludeFiles:  false,
		VettedCount:   0,
		BranchesCount: 0,
	}

	responseBody, err := b.makeRequest(http.MethodPost, pd.InventoryRoute, inv)
	if err != nil {
		return nil, err
	}

	var ir pd.InventoryReply
	err = json.Unmarshal(responseBody, &ir)
	if err != nil {
		return nil, fmt.Errorf("Unmarshal InventoryReply: %v",
			err)
	}

	err = util.VerifyChallenge(b.cfg.Identity, challenge, ir.Response)
	if err != nil {
		return nil, err
	}

	return &ir, nil
}

func (b *backend) validatePassword(password string) error {
	if len(password) < www.PolicyPasswordMinChars {
		return userError{
			errorCode: www.StatusMalformedPassword,
		}
	}

	return nil
}

func (b *backend) validateProposal(np www.NewProposal) error {
	// Check for a non-empty name.
	if np.Name == "" {
		return userError{
			errorCode: www.StatusProposalMissingName,
		}
	}

	// Check for at least 1 markdown file with a non-emtpy payload.
	if len(np.Files) == 0 || np.Files[0].Payload == "" {
		return userError{
			errorCode: www.StatusProposalMissingDescription,
		}
	}

	// Check that the file number policy is followed.
	var numMDs, numImages uint = 0, 0
	var mdExceedsMaxSize, imageExceedsMaxSize bool = false, false
	for _, v := range np.Files {
		if strings.HasPrefix(v.MIME, "image/") {
			numImages++
			data, err := base64.StdEncoding.DecodeString(v.Payload)
			if err != nil {
				return err
			}
			if len(data) > www.PolicyMaxImageSize {
				imageExceedsMaxSize = true
			}
		} else {
			numMDs++
			data, err := base64.StdEncoding.DecodeString(v.Payload)
			if err != nil {
				return err
			}
			if len(data) > www.PolicyMaxMDSize {
				mdExceedsMaxSize = true
			}
		}
	}

	if numMDs > www.PolicyMaxMDs {
		return userError{
			errorCode: www.StatusMaxMDsExceededPolicy,
		}
	}

	if numImages > www.PolicyMaxImages {
		return userError{
			errorCode: www.StatusMaxImagesExceededPolicy,
		}
	}

	if mdExceedsMaxSize {
		return userError{
			errorCode: www.StatusMaxMDSizeExceededPolicy,
		}
	}

	if imageExceedsMaxSize {
		return userError{
			errorCode: www.StatusMaxImageSizeExceededPolicy,
		}
	}

	return nil
}

func (b *backend) emailResetPassword(user *database.User, rp www.ResetPassword, rpr *www.ResetPasswordReply) error {
	if user.ResetPasswordVerificationToken != nil {
		currentTime := time.Now().Unix()
		if currentTime < user.ResetPasswordVerificationExpiry {
			// The verification token is present and hasn't expired, so do nothing.
			return nil
		}
	}

	// The verification token isn't present or is present but expired.

	// Generate a new verification token and expiry.
	token, expiry, err := b.generateVerificationTokenAndExpiry()
	if err != nil {
		return err
	}

	// Add the updated user information to the db.
	user.ResetPasswordVerificationToken = token
	user.ResetPasswordVerificationExpiry = expiry
	err = b.db.UserUpdate(*user)
	if err != nil {
		return err
	}

	if !b.test {
		// This is conditional on the email server being setup.
		err := b.emailResetPasswordVerificationLink(rp.Email, hex.EncodeToString(token))
		if err != nil {
			return err
		}
	}

	// Only set the token if email verification is disabled.
	if b.cfg.SMTP == nil {
		rpr.VerificationToken = hex.EncodeToString(token)
	}

	return nil
}

func (b *backend) verifyResetPassword(user *database.User, rp www.ResetPassword, rpr *www.ResetPasswordReply) error {
	// Decode the verification token.
	token, err := hex.DecodeString(rp.VerificationToken)
	if err != nil {
		return userError{
			errorCode: www.StatusVerificationTokenInvalid,
		}
	}

	// Check that the verification token matches.
	if !bytes.Equal(token, user.ResetPasswordVerificationToken) {
		return userError{
			errorCode: www.StatusVerificationTokenInvalid,
		}
	}

	// Check that the token hasn't expired.
	currentTime := time.Now().Unix()
	if currentTime > user.ResetPasswordVerificationExpiry {
		return userError{
			errorCode: www.StatusVerificationTokenExpired,
		}
	}

	// Validate the new password.
	err = b.validatePassword(rp.NewPassword)
	if err != nil {
		return err
	}

	// Hash the new password.
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(rp.NewPassword),
		bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	// Clear out the verification token fields and set the new password in the db.
	user.NewUserVerificationToken = nil
	user.NewUserVerificationExpiry = 0
	user.HashedPassword = hashedPassword

	return b.db.UserUpdate(*user)
}

// LoadInventory fetches the entire inventory of proposals from politeiad
// and caches it, sorted by most recent timestamp.
func (b *backend) LoadInventory() error {
	var inv *pd.InventoryReply
	if b.test {
		// Split the existing inventory into vetted and unvetted.
		vetted := make([]www.ProposalRecord, 0)
		unvetted := make([]www.ProposalRecord, 0)

		for _, v := range b.inventory {
			if v.Status == www.PropStatusPublic {
				vetted = append(vetted, v)
			} else {
				unvetted = append(unvetted, v)
			}
		}

		inv = &pd.InventoryReply{
			Vetted:   convertPropsFromWWW(vetted),
			Branches: convertPropsFromWWW(unvetted),
		}
	} else {
		// Fetch remote inventory.
		var err error
		inv, err = b.remoteInventory()
		if err != nil {
			return fmt.Errorf("LoadInventory: %v", err)
		}

		log.Infof("Adding %v vetted, %v unvetted proposals to the cache",
			len(inv.Vetted), len(inv.Branches))
	}

	b.inventory = make([]www.ProposalRecord, 0, len(inv.Vetted)+len(inv.Branches))
	for _, vv := range append(inv.Vetted, inv.Branches...) {
		v := convertPropFromPD(vv)
		len := len(b.inventory)
		if len == 0 {
			b.inventory = append(b.inventory, v)
		} else {
			idx := sort.Search(len, func(i int) bool {
				return v.Timestamp < b.inventory[i].Timestamp
			})

			// Insert the proposal at idx.
			b.inventory = append(b.inventory[:idx],
				append([]www.ProposalRecord{v},
					b.inventory[idx:]...)...)
		}
	}

	return nil
}

// ProcessNewUser creates a new user in the db if it doesn't already
// exist and sets a verification token and expiry; the token must be
// verified before it expires. If the user already exists in the db
// and its token is expired, it generates a new one.
//
// Note that this function always returns a NewUserReply.  The caller shally
// verify error and determine how to return this information upstream.
func (b *backend) ProcessNewUser(u www.NewUser) (*www.NewUserReply, error) {
	var reply www.NewUserReply
	var token []byte
	var expiry int64

	// XXX this function really needs to be cleaned up.
	// XXX We should create a sinlge reply struct that get's returned
	// instead of many.

	// Check if the user already exists.
	if user, err := b.db.UserGet(u.Email); err == nil {
		// Check if the user is already verified.
		if user.NewUserVerificationToken == nil {
			reply.ErrorCode = www.StatusSuccess
			return &reply, nil
		}

		// Check if the verification token hasn't expired yet.
		if currentTime := time.Now().Unix(); currentTime < user.NewUserVerificationExpiry {
			reply.ErrorCode = www.StatusSuccess
			return &reply, nil
		}

		// Generate a new verification token and expiry.
		token, expiry, err = b.generateVerificationTokenAndExpiry()
		if err != nil {
			return nil, err
		}

		// Add the updated user information to the db.
		user.NewUserVerificationToken = token
		user.NewUserVerificationExpiry = expiry
		err = b.db.UserUpdate(*user)
		if err != nil {
			return nil, err
		}
	} else {
		// Validate the password.
		err = b.validatePassword(u.Password)
		if err != nil {
			return nil, err
		}

		// Hash the user's password.
		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(u.Password),
			bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}

		// Generate the verification token and expiry.
		token, expiry, err = b.generateVerificationTokenAndExpiry()
		if err != nil {
			return nil, err
		}

		// Add the user and hashed password to the db.
		newUser := database.User{
			Email:          u.Email,
			HashedPassword: hashedPassword,
			Admin:          false,
			NewUserVerificationToken:  token,
			NewUserVerificationExpiry: expiry,
		}

		err = b.db.UserNew(newUser)
		if err != nil {
			if err == database.ErrInvalidEmail {
				return nil, userError{
					errorCode: www.StatusMalformedEmail,
				}
			}

			return nil, err
		}
	}

	if !b.test {
		// This is conditional on the email server being setup.
		err := b.emailNewUserVerificationLink(u.Email, hex.EncodeToString(token))
		if err != nil {
			return nil, err
		}
	}

	reply.ErrorCode = www.StatusSuccess

	// Only set the token if email verification is disabled.
	if b.cfg.SMTP == nil {
		reply.VerificationToken = hex.EncodeToString(token)
	}
	return &reply, nil
}

// ProcessVerifyNewUser verifies the token generated for a recently created user.
// It ensures that the token matches with the input and that the token hasn't expired.
func (b *backend) ProcessVerifyNewUser(u www.VerifyNewUser) error {
	// Check that the user already exists.
	user, err := b.db.UserGet(u.Email)
	if err != nil {
		if err == database.ErrUserNotFound {
			return userError{
				errorCode: www.StatusVerificationTokenInvalid,
			}
		}
		return err
	}

	// Decode the verification token.
	token, err := hex.DecodeString(u.VerificationToken)
	if err != nil {
		return userError{
			errorCode: www.StatusVerificationTokenInvalid,
		}
	}

	// Check that the verification token matches.
	if !bytes.Equal(token, user.NewUserVerificationToken) {
		return userError{
			errorCode: www.StatusVerificationTokenInvalid,
		}
	}

	// Check that the token hasn't expired.
	if currentTime := time.Now().Unix(); currentTime > user.NewUserVerificationExpiry {
		return userError{
			errorCode: www.StatusVerificationTokenExpired,
		}
	}

	// Clear out the verification token fields in the db.
	user.NewUserVerificationToken = nil
	user.NewUserVerificationExpiry = 0
	return b.db.UserUpdate(*user)
}

// ProcessLogin checks that a user exists, is verified, and has
// the correct password.
func (b *backend) ProcessLogin(l www.Login) (*www.LoginReply, error) {
	var reply www.LoginReply

	// Get user from db.
	user, err := b.db.UserGet(l.Email)
	if err != nil {
		if err == database.ErrUserNotFound {
			return nil, userError{
				errorCode: www.StatusInvalidEmailOrPassword,
			}
		}
		return nil, err
	}

	// Check that the user is verified.
	if user.NewUserVerificationToken != nil {
		return nil, userError{
			errorCode: www.StatusInvalidEmailOrPassword,
		}
	}

	// Check the user's password.
	err = bcrypt.CompareHashAndPassword(user.HashedPassword,
		[]byte(l.Password))
	if err != nil {
		return nil, userError{
			errorCode: www.StatusInvalidEmailOrPassword,
		}
	}

	reply.IsAdmin = user.Admin
	reply.ErrorCode = www.StatusSuccess
	return &reply, nil
}

// ProcessChangePassword checks that the current password matches the one
// in the database, then changes it to the new password.
func (b *backend) ProcessChangePassword(email string, cp www.ChangePassword) (*www.ChangePasswordReply, error) {
	var reply www.ChangePasswordReply

	// Get user from db.
	user, err := b.db.UserGet(email)
	if err != nil {
		return nil, err
	}

	// Check the user's password.
	err = bcrypt.CompareHashAndPassword(user.HashedPassword,
		[]byte(cp.CurrentPassword))
	if err != nil {
		return nil, userError{
			errorCode: www.StatusInvalidEmailOrPassword,
		}
	}

	// Validate the new password.
	err = b.validatePassword(cp.NewPassword)
	if err != nil {
		return nil, err
	}

	// Hash the user's password.
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(cp.NewPassword),
		bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	// Add the updated user information to the db.
	user.HashedPassword = hashedPassword
	err = b.db.UserUpdate(*user)
	if err != nil {
		return nil, err
	}

	reply.ErrorCode = www.StatusSuccess
	return &reply, nil
}

// ProcessResetPassword is intended to be called twice; in the first call, an
// email is provided and the function checks if the user exists. If the user exists, it
// generates a verification token and stores it in the database. In the second
// call, the email, verification token and a new password are provided. If everything
// matches, then the user's password is updated in the database.
func (b *backend) ProcessResetPassword(rp www.ResetPassword) (*www.ResetPasswordReply, error) {
	var reply www.ResetPasswordReply

	// Get user from db.
	user, err := b.db.UserGet(rp.Email)
	if err != nil {
		if err == database.ErrInvalidEmail {
			return nil, userError{
				errorCode: www.StatusMalformedEmail,
			}
		} else if err == database.ErrUserNotFound {
			reply.ErrorCode = www.StatusSuccess
			return &reply, nil
		}

		return nil, err
	}

	if rp.VerificationToken == "" {
		err = b.emailResetPassword(user, rp, &reply)
	} else {
		err = b.verifyResetPassword(user, rp, &reply)
	}

	if err != nil {
		return nil, err
	}

	reply.ErrorCode = www.StatusSuccess
	return &reply, nil
}

// ProcessAllVetted returns an array of all vetted proposals in reverse order,
// because they're sorted by oldest timestamp first.
func (b *backend) ProcessAllVetted(v www.GetAllVetted) *www.GetAllVettedReply {
	proposals := make([]www.ProposalRecord, 0)
	for i := len(b.inventory) - 1; i >= 0; i-- {
		if b.inventory[i].Status == www.PropStatusPublic {
			proposals = append(proposals, b.inventory[i])
		}
	}

	return &www.GetAllVettedReply{
		Proposals: proposals,
		ErrorCode: www.StatusSuccess,
	}
}

// ProcessAllUnvetted returns an array of all unvetted proposals in reverse order,
// because they're sorted by oldest timestamp first.
func (b *backend) ProcessAllUnvetted(u www.GetAllUnvetted) *www.GetAllUnvettedReply {
	proposals := make([]www.ProposalRecord, 0)
	for i := len(b.inventory) - 1; i >= 0; i-- {
		if b.inventory[i].Status == www.PropStatusNotReviewed ||
			b.inventory[i].Status == www.PropStatusCensored {
			proposals = append(proposals, b.inventory[i])
		}
	}

	return &www.GetAllUnvettedReply{
		Proposals: proposals,
		ErrorCode: www.StatusSuccess,
	}
}

// ProcessNewProposal tries to submit a new proposal to politeiad.
func (b *backend) ProcessNewProposal(np www.NewProposal) (*www.NewProposalReply, error) {
	var reply www.NewProposalReply

	err := b.validateProposal(np)
	if err != nil {
		return nil, err
	}

	challenge, err := util.Random(pd.ChallengeSize)
	if err != nil {
		return nil, err
	}

	n := pd.New{
		Name:      sanitize.Name(np.Name),
		Challenge: hex.EncodeToString(challenge),
		Files:     convertPropFilesFromWWW(np.Files),
	}

	for k, f := range n.Files {
		decodedPayload, err := base64.StdEncoding.DecodeString(f.Payload)
		if err != nil {
			return nil, err
		}

		// Calculate the digest for each file.
		h := sha256.New()
		h.Write(decodedPayload)
		n.Files[k].Digest = hex.EncodeToString(h.Sum(nil))
	}

	var pdReply pd.NewReply
	if b.test {
		tokenBytes, err := util.Random(16)
		if err != nil {
			return nil, err
		}

		pdReply.Timestamp = time.Now().Unix()
		pdReply.CensorshipRecord = pd.CensorshipRecord{
			Token: hex.EncodeToString(tokenBytes),
		}

		// Add the new proposal to the cache.
		b.inventory = append(b.inventory, www.ProposalRecord{
			Name:             np.Name,
			Status:           www.PropStatusNotReviewed,
			Timestamp:        pdReply.Timestamp,
			Files:            np.Files,
			CensorshipRecord: convertPropCensorFromPD(pdReply.CensorshipRecord),
		})
	} else {
		responseBody, err := b.makeRequest(http.MethodPost, pd.NewRoute, n)
		if err != nil {
			return nil, err
		}

		fmt.Printf("Submitted proposal name: %v\n", n.Name)
		for k, f := range n.Files {
			fmt.Printf("%02v: %v %v\n", k, f.Name, f.Digest)
		}

		err = json.Unmarshal(responseBody, &pdReply)
		if err != nil {
			return nil, fmt.Errorf("Unmarshal NewProposalReply: %v",
				err)
		}

		// Verify the challenge.
		err = util.VerifyChallenge(b.cfg.Identity, challenge, pdReply.Response)
		if err != nil {
			return nil, err
		}

		// Add the new proposal to the cache.
		b.inventory = append(b.inventory, www.ProposalRecord{
			Name:             np.Name,
			Status:           www.PropStatusNotReviewed,
			Timestamp:        pdReply.Timestamp,
			Files:            make([]www.File, 0),
			CensorshipRecord: convertPropCensorFromPD(pdReply.CensorshipRecord),
		})
	}

	reply.CensorshipRecord = convertPropCensorFromPD(pdReply.CensorshipRecord)
	reply.ErrorCode = www.StatusSuccess
	return &reply, nil
}

// ProcessSetProposalStatus changes the status of an existing proposal
// from unreviewed to either published or censored.
func (b *backend) ProcessSetProposalStatus(sps www.SetProposalStatus) (*www.SetProposalStatusReply, error) {
	var reply www.SetProposalStatusReply
	var pdReply pd.SetUnvettedStatusReply
	if b.test {
		pdReply.Status = convertPropStatusFromWWW(sps.ProposalStatus)
	} else {
		challenge, err := util.Random(pd.ChallengeSize)
		if err != nil {
			return nil, err
		}

		sus := pd.SetUnvettedStatus{
			Token:     sps.Token,
			Status:    convertPropStatusFromWWW(sps.ProposalStatus),
			Challenge: hex.EncodeToString(challenge),
		}

		responseBody, err := b.makeRequest(http.MethodPost,
			pd.SetUnvettedStatusRoute, sus)
		if err != nil {
			return nil, err
		}

		err = json.Unmarshal(responseBody, &pdReply)
		if err != nil {
			return nil, fmt.Errorf("Could not unmarshal SetUnvettedStatusReply: %v",
				err)
		}

		// Verify the challenge.
		err = util.VerifyChallenge(b.cfg.Identity, challenge, pdReply.Response)
		if err != nil {
			return nil, err
		}
	}

	// Update the cached proposal with the new status and return the reply.
	for k, v := range b.inventory {
		if v.CensorshipRecord.Token == sps.Token {
			s := convertPropStatusFromPD(pdReply.Status)
			b.inventory[k].Status = s
			reply.ProposalStatus = s
			reply.ErrorCode = www.StatusSuccess
			return &reply, nil
		}
	}

	return nil, userError{
		errorCode: www.StatusProposalNotFound,
	}
}

// ProcessProposalDetails tries to fetch the full details of a proposal from politeiad.
func (b *backend) ProcessProposalDetails(propDetails www.ProposalsDetails, isUserAdmin bool) (*www.ProposalDetailsReply, error) {
	var reply www.ProposalDetailsReply
	challenge, err := util.Random(pd.ChallengeSize)
	if err != nil {
		return nil, err
	}

	var cachedProposal *www.ProposalRecord
	for _, v := range b.inventory {
		if v.CensorshipRecord.Token == propDetails.Token {
			cachedProposal = &v
			break
		}
	}
	if cachedProposal == nil {
		return nil, userError{
			errorCode: www.StatusProposalNotFound,
		}
	}

	var isVettedProposal bool
	var requestObject interface{}
	if cachedProposal.Status == www.PropStatusPublic {
		isVettedProposal = true
		requestObject = pd.GetVetted{
			Token:     propDetails.Token,
			Challenge: hex.EncodeToString(challenge),
		}
	} else {
		isVettedProposal = false
		requestObject = pd.GetUnvetted{
			Token:     propDetails.Token,
			Challenge: hex.EncodeToString(challenge),
		}
	}

	if b.test {
		reply.ErrorCode = www.StatusSuccess
		reply.Proposal = *cachedProposal
		return &reply, nil
	}

	// The title and files for unvetted proposals should not be viewable by
	// non-admins; only the proposal meta data (status, censorship data, etc)
	// should be publicly viewable.
	if !isVettedProposal && !isUserAdmin {
		reply.ErrorCode = www.StatusSuccess
		reply.Proposal = www.ProposalRecord{
			Status:           cachedProposal.Status,
			Timestamp:        cachedProposal.Timestamp,
			CensorshipRecord: cachedProposal.CensorshipRecord,
		}
		return &reply, nil
	}

	var route string
	if isVettedProposal {
		route = pd.GetVettedRoute
	} else {
		route = pd.GetUnvettedRoute
	}

	responseBody, err := b.makeRequest(http.MethodPost, route, requestObject)
	if err != nil {
		return nil, err
	}

	var response string
	var proposal pd.ProposalRecord
	if isVettedProposal {
		var pdReply pd.GetVettedReply
		err = json.Unmarshal(responseBody, &pdReply)
		if err != nil {
			return nil, fmt.Errorf("Could not unmarshal "+
				"GetVettedReply: %v", err)
		}

		response = pdReply.Response
		proposal = pdReply.Proposal
	} else {
		var pdReply pd.GetUnvettedReply
		err = json.Unmarshal(responseBody, &pdReply)
		if err != nil {
			return nil, fmt.Errorf("Could not unmarshal "+
				"GetUnvettedReply: %v", err)
		}

		response = pdReply.Response
		proposal = pdReply.Proposal
	}

	// Verify the challenge.
	err = util.VerifyChallenge(b.cfg.Identity, challenge, response)
	if err != nil {
		return nil, err
	}

	reply.ErrorCode = www.StatusSuccess
	reply.Proposal = convertPropFromPD(proposal)
	return &reply, nil
}

// ProcessPolicy returns the details of Politeia's restrictions on file uploads.
func (b *backend) ProcessPolicy(p www.Policy) *www.PolicyReply {
	return &www.PolicyReply{
		PasswordMinChars: www.PolicyPasswordMinChars,
		MaxImages:        www.PolicyMaxImages,
		MaxImageSize:     www.PolicyMaxImageSize,
		MaxMDs:           www.PolicyMaxMDs,
		MaxMDSize:        www.PolicyMaxMDSize,
		ValidMIMETypes:   mime.ValidMimeTypes(),
		ErrorCode:        www.StatusSuccess,
	}
}

// NewBackend creates a new backend context for use in www and tests.
func NewBackend(cfg *config) (*backend, error) {
	// Setup database.
	localdb.UseLogger(localdbLog)
	db, err := localdb.New(cfg.DataDir)
	if err != nil {
		return nil, err
	}

	b := &backend{
		db:  db,
		cfg: cfg,
	}
	return b, nil
}
