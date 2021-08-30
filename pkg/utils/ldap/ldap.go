package ldap

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"net"
	"strconv"
	"strings"

	"gitee.com/golden-go/golden-go/pkg/models"
	"gitee.com/golden-go/golden-go/pkg/utils/logger"
	"gitee.com/golden-go/golden-go/pkg/utils/types"
	"github.com/davecgh/go-spew/spew"
	goldap "github.com/go-ldap/ldap"
	"go.uber.org/zap"
)

// Config holds list of connections to LDAP
type Config struct {
	Servers []*ServerConfig `json:"servers"`
}

// ServerConfig holds connection data to LDAP
type ServerConfig struct {
	Host          string       `json:"host"`
	Port          int          `json:"port"`
	UseSSL        bool         `json:"use_ssl"`
	StartTLS      bool         `json:"start_tls"`
	SkipVerifySSL bool         `json:"ssl_skip_verify"`
	RootCACert    string       `json:"root_ca_cert"`
	ClientCert    string       `json:"client_cert"`
	ClientKey     string       `json:"client_key"`
	BindDN        string       `json:"bind_dn"`
	BindPassword  string       `json:"bind_password"`
	Attr          AttributeMap `json:"attributes"`

	SearchFilter  string   `json:"search_filter"`
	SearchBaseDNs []string `json:"search_base_dns"`

	GroupSearchFilter              string   `json:"group_search_filter"`
	GroupSearchFilterUserAttribute string   `json:"group_search_filter_user_attribute"`
	GroupSearchBaseDNs             []string `json:"group_search_base_dns"`

	//Groups []*GroupToOrgRole `json:"group_mappings"`
}

// AttributeMap is a struct representation for LDAP "attributes" setting
type AttributeMap struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	Surname  string `json:"surname"`
	Email    string `json:"email"`
	MemberOf string `json:"member_of"`
}

// GroupToOrgRole is a struct representation of LDAP
// config "group_mappings" setting
//type GroupToOrgRole struct {
//	GroupDN string `json:"group_dn"`
//	OrgId   int64  `json:"org_id"`
//
//	// This pointer specifies if setting was set (for backwards compatibility)
//	IsGrafanaAdmin *bool `json:"grafana_admin"`
//
//	OrgRole models.RoleType `json:"org_role"`
//}

// func isMemberOf(memberOf []string, group string) bool {
// 	if group == "*" {
// 		return true
// 	}

// 	for _, member := range memberOf {
// 		if strings.EqualFold(member, group) {
// 			return true
// 		}
// 	}
// 	return false
// }

func appendIfNotEmpty(slice []string, values ...string) []string {
	for _, v := range values {
		if v != "" {
			slice = append(slice, v)
		}
	}
	return slice
}

func getAttribute(name string, entry *goldap.Entry) string {
	if strings.ToLower(name) == "dn" {
		return entry.DN
	}

	for _, attr := range entry.Attributes {
		if attr.Name == name {
			if len(attr.Values) > 0 {
				return attr.Values[0]
			}
		}
	}
	return ""
}

func getArrayAttribute(name string, entry *goldap.Entry) []string {
	if strings.ToLower(name) == "dn" {
		return []string{entry.DN}
	}

	for _, attr := range entry.Attributes {
		if attr.Name == name && len(attr.Values) > 0 {
			return attr.Values
		}
	}
	return []string{}
}

//LDAP 连接服务端接口interface
type IConnection interface {
	Bind(username, password string) error
	UnauthenticatedBind(username string) error
	Add(*goldap.AddRequest) error
	Del(*goldap.DelRequest) error
	Search(*goldap.SearchRequest) (*goldap.SearchResult, error)
	StartTLS(*tls.Config) error
	Close()
}

// IServer LDAP 服务端认证接口interface
type IServer interface {
	Login(data *types.LoginData) (*models.User, error)
	Users([]string) ([]*models.User, error)
	Bind() error
	UserBind(string, string) error
	Dial() error
	Close()
}

// Server is basic struct of LDAP authorization
type Server struct {
	Config     *ServerConfig
	Connection IConnection
}

// Bind authenticates the connection with the LDAP server
// - with the username and password setup in the config
// - or, anonymously
//
// Dial() sets the connection with the server for this Struct. Therefore, we require a
// call to Dial() before being able to execute this function.
func (server *Server) Bind() error {
	if server.shouldAdminBind() {
		if err := server.AdminBind(); err != nil {
			return err
		}
	} else {
		err := server.Connection.UnauthenticatedBind(server.Config.BindDN)
		if err != nil {
			return err
		}
	}
	return nil
}

// UsersMaxRequest is a max amount of users we can request via Users().
// Since many LDAP servers has limitations
// on how much items can we return in one request
const UsersMaxRequest = 500

var (

	// ErrInvalidCredentials is returned if username and password do not match
	ErrInvalidCredentials = errors.New("invalid username or password")

	// ErrCouldNotFindUser is returned when username hasn't been found (not username+password)
	ErrCouldNotFindUser = errors.New("can't find user in LDAP")
)

// New creates the new LDAP connection
func NewLDAPServer(config *ServerConfig) IServer {
	return &Server{
		Config: config,
	}
}

// Dial dials in the LDAP
// TODO: decrease cyclomatic complexity
func (server *Server) Dial() error {
	var err error
	var certPool *x509.CertPool
	if server.Config.RootCACert != "" {
		certPool = x509.NewCertPool()
		for _, caCertFile := range strings.Split(server.Config.RootCACert, " ") {
			// nolint:gosec
			// We can ignore the gosec G304 warning on this one because `caCertFile` comes from ldap config.
			pem, err := ioutil.ReadFile(caCertFile)
			if err != nil {
				return err
			}
			if !certPool.AppendCertsFromPEM(pem) {
				return errors.New("Failed to append CA certificate " + caCertFile)
			}
		}
	}
	var clientCert tls.Certificate
	if server.Config.ClientCert != "" && server.Config.ClientKey != "" {
		clientCert, err = tls.LoadX509KeyPair(server.Config.ClientCert, server.Config.ClientKey)
		if err != nil {
			return err
		}
	}
	for _, host := range strings.Split(server.Config.Host, " ") {
		// Remove any square brackets enclosing IPv6 addresses, a format we support for backwards compatibility
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		address := net.JoinHostPort(host, strconv.Itoa(server.Config.Port))
		if server.Config.UseSSL {
			tlsCfg := &tls.Config{
				InsecureSkipVerify: server.Config.SkipVerifySSL,
				ServerName:         host,
				RootCAs:            certPool,
			}
			if len(clientCert.Certificate) > 0 {
				tlsCfg.Certificates = append(tlsCfg.Certificates, clientCert)
			}
			if server.Config.StartTLS {
				server.Connection, err = goldap.Dial("tcp", address)
				if err == nil {
					if err = server.Connection.StartTLS(tlsCfg); err == nil {
						return nil
					}
				}
			} else {
				server.Connection, err = goldap.DialTLS("tcp", address, tlsCfg)
			}
		} else {
			server.Connection, err = goldap.Dial("tcp", address)
		}

		if err == nil {
			return nil
		}
	}
	return err
}

// Close closes the LDAP connection
// Dial() sets the connection with the server for this Struct. Therefore, we require a
// call to Dial() before being able to execute this function.
func (server *Server) Close() {
	server.Connection.Close()
}

// Login the user.
// There are several cases -
// 1. "admin" user
// Bind the "admin" user (defined in Grafana config file) which has the search privileges
// in LDAP server, then we search the targeted user through that bind, then the second
// perform the bind via passed login/password.
// 2. Single bind
// // If all the users meant to be used with Grafana have the ability to search in LDAP server
// then we bind with LDAP server with targeted login/password
// and then search for the said user in order to retrieve all the information about them
// 3. Unauthenticated bind
// For some LDAP configurations it is allowed to search the
// user without login/password binding with LDAP server, in such case
// we will perform "unauthenticated bind", then search for the
// targeted user and then perform the bind with passed login/password.
//
// Dial() sets the connection with the server for this Struct. Therefore, we require a
// call to Dial() before being able to execute this function.
func (server *Server) Login(query *types.LoginData) (
	*models.User, error,
) {
	var err error
	var authAndBind bool

	// Check if we can use a search user
	switch {
	case server.shouldAdminBind():
		if err := server.AdminBind(); err != nil {
			return nil, err
		}
	case server.shouldSingleBind():
		authAndBind = true
		err = server.UserBind(
			server.singleBindDN(query.Name),
			query.Password,
		)
		if err != nil {
			return nil, err
		}
	default:
		err := server.Connection.UnauthenticatedBind(server.Config.BindDN)
		if err != nil {
			return nil, err
		}
	}

	// Find user entry & attributes
	users, err := server.Users([]string{query.Name})
	if err != nil {
		return nil, err
	}

	// If we couldn't find the user -
	// we should show incorrect credentials err
	if len(users) == 0 {
		return nil, ErrCouldNotFindUser
	}

	user := users[0]
	if err := server.validateGoldenUser(user); err != nil {
		return nil, err
	}

	if !authAndBind {
		// Authenticate user
		err = server.UserBind(user.Name, query.Password)
		if err != nil {
			return nil, err
		}
	}

	return user, nil
}

// shouldAdminBind checks if we should use
// admin username & password for LDAP bind
func (server *Server) shouldAdminBind() bool {
	return server.Config.BindPassword != ""
}

// singleBindDN combines the bind with the username
// in order to get the proper path
func (server *Server) singleBindDN(username string) string {
	return fmt.Sprintf(server.Config.BindDN, username)
}

// shouldSingleBind checks if we can use "single bind" approach
func (server *Server) shouldSingleBind() bool {
	return strings.Contains(server.Config.BindDN, "%s")
}

// Users gets LDAP users by logins
// Dial() sets the connection with the server for this Struct. Therefore, we require a
// call to Dial() before being able to execute this function.
func (server *Server) Users(logins []string) (
	[]*models.User,
	error,
) {
	var users []*goldap.Entry
	err := getUsersIteration(logins, func(previous, current int) error {
		entries, err := server.users(logins[previous:current])
		if err != nil {
			return err
		}

		users = append(users, entries...)

		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(users) == 0 {
		return []*models.User{}, nil
	}

	serializedUsers, err := server.serializeUsers(users)
	if err != nil {
		return nil, err
	}

	logger.Debug(
		"LDAP users found", zap.String("users", spew.Sdump(serializedUsers)),
		zap.Any("ldap_users", users),
	)

	return serializedUsers, nil
}

// getUsersIteration is a helper function for Users() method.
// It divides the users by equal parts for the anticipated requests
func getUsersIteration(logins []string, fn func(int, int) error) error {
	lenLogins := len(logins)
	iterations := int(
		math.Ceil(
			float64(lenLogins) / float64(UsersMaxRequest),
		),
	)

	for i := 1; i < iterations+1; i++ {
		previous := float64(UsersMaxRequest * (i - 1))
		current := math.Min(float64(i*UsersMaxRequest), float64(lenLogins))

		err := fn(int(previous), int(current))
		if err != nil {
			return err
		}
	}

	return nil
}

// users is helper method for the Users()
func (server *Server) users(logins []string) (
	[]*goldap.Entry,
	error,
) {
	var result *goldap.SearchResult
	var Config = server.Config
	var err error

	for _, base := range Config.SearchBaseDNs {
		result, err = server.Connection.Search(
			server.getSearchRequest(base, logins),
		)
		if err != nil {
			return nil, err
		}

		if len(result.Entries) > 0 {
			break
		}
	}

	return result.Entries, nil
}

// validateGoldenUser validates user access.
// If there are no ldap group mappings access is true
// otherwise a single group must match
func (server *Server) validateGoldenUser(user *models.User) error {
	/*	if len(server.Config.Groups) > 0 && len(user.OrgRoles) < 1 {
		logger.Error(
			"User does not belong in any of the specified LDAP groups",
			"username", user.Login,
			"groups", user.Groups,
		)
		return ErrInvalidCredentials
	}*/

	return nil
}

// getSearchRequest returns LDAP search request for users
func (server *Server) getSearchRequest(
	base string,
	logins []string,
) *goldap.SearchRequest {
	attributes := []string{}

	inputs := server.Config.Attr
	attributes = appendIfNotEmpty(
		attributes,
		inputs.Username,
		inputs.Surname,
		inputs.Email,
		inputs.Name,
		inputs.MemberOf,

		// In case for the POSIX LDAP schema server
		server.Config.GroupSearchFilterUserAttribute,
	)

	search := ""
	for _, login := range logins {
		query := strings.ReplaceAll(
			server.Config.SearchFilter,
			"%s", goldap.EscapeFilter(login),
		)

		search += query
	}

	filter := fmt.Sprintf("(|%s)", search)

	searchRequest := &goldap.SearchRequest{
		BaseDN:       base,
		Scope:        goldap.ScopeWholeSubtree,
		DerefAliases: goldap.NeverDerefAliases,
		Attributes:   attributes,
		Filter:       filter,
	}

	logger.Debug(
		"LDAP SearchRequest", zap.String("searchRequest", fmt.Sprintf("%+v\n", searchRequest)),
	)

	return searchRequest
}

// buildGoldenUser extracts info from UserInfo model to ExternalUserInfo
func (server *Server) buildGoldenUser(user *goldap.Entry) (*models.User, error) {
	/*	memberOf, err := server.getMemberOf(user)
		if err != nil {
			return nil, err
		}*/

	attrs := server.Config.Attr
	extUser := &models.User{
		AuthModule: models.AuthModuleLDAP,
		Name: strings.TrimSpace(
			fmt.Sprintf(
				"%s %s",
				getAttribute(attrs.Name, user),
				getAttribute(attrs.Surname, user),
			),
		),
		//Login:    getAttribute(attrs.Username, user),
		Email: getAttribute(attrs.Email, user),
		/*		Groups:   memberOf,
				OrgRoles: map[int64]models.RoleType{},*/
	}

	/*	for _, group := range server.Config.Groups {
			// only use the first match for each org
			if extUser.OrgRoles[group.OrgId] != "" {
				continue
			}

			if isMemberOf(memberOf, group.GroupDN) {
				extUser.OrgRoles[group.OrgId] = group.OrgRole
				if extUser.IsGrafanaAdmin == nil || !*extUser.IsGrafanaAdmin {
					extUser.IsGrafanaAdmin = group.IsGrafanaAdmin
				}
			}
		}

		// If there are group org mappings configured, but no matching mappings,
		// the user will not be able to login and will be disabled
		if len(server.Config.Groups) > 0 && len(extUser.OrgRoles) == 0 {
			extUser.IsDisabled = true
		}*/

	return extUser, nil
}

// UserBind binds the user with the LDAP server
// Dial() sets the connection with the server for this Struct. Therefore, we require a
// call to Dial() before being able to execute this function.
func (server *Server) UserBind(username, password string) error {
	err := server.userBind(username, password)
	if err != nil {
		logger.Error(
			fmt.Sprintf("Cannot bind user %s with LDAP", username),
			zap.Error(err),
		)
		return err
	}

	return nil
}

// AdminBind binds "admin" user with LDAP
// Dial() sets the connection with the server for this Struct. Therefore, we require a
// call to Dial() before being able to execute this function.
func (server *Server) AdminBind() error {
	err := server.userBind(server.Config.BindDN, server.Config.BindPassword)
	if err != nil {
		logger.Error(
			"Cannot authenticate admin user in LDAP",
			zap.Error(err),
		)
		return err
	}

	return nil
}

// userBind binds the user with the LDAP server
func (server *Server) userBind(path, password string) error {
	err := server.Connection.Bind(path, password)
	if err != nil {
		var ldapErr *goldap.Error
		if errors.As(err, &ldapErr) && ldapErr.ResultCode == 49 {
			return ErrInvalidCredentials
		}

		return err
	}

	return nil
}

// requestMemberOf use this function when POSIX LDAP
// schema does not support memberOf, so it manually search the groups
func (server *Server) requestMemberOf(entry *goldap.Entry) ([]string, error) {
	var memberOf []string
	var config = server.Config
	var searchBaseDNs []string

	if len(config.GroupSearchBaseDNs) > 0 {
		searchBaseDNs = config.GroupSearchBaseDNs
	} else {
		searchBaseDNs = config.SearchBaseDNs
	}

	for _, groupSearchBase := range searchBaseDNs {
		var filterReplace string
		if config.GroupSearchFilterUserAttribute == "" {
			filterReplace = getAttribute(config.Attr.Username, entry)
		} else {
			filterReplace = getAttribute(
				config.GroupSearchFilterUserAttribute,
				entry,
			)
		}

		filter := strings.ReplaceAll(
			config.GroupSearchFilter, "%s",
			goldap.EscapeFilter(filterReplace),
		)
		logger.Info("Searching for user's groups", zap.String("filter", filter))

		// support old way of reading settings
		groupIDAttribute := config.Attr.MemberOf
		// but prefer dn attribute if default settings are used
		if groupIDAttribute == "" || groupIDAttribute == "memberOf" {
			groupIDAttribute = "dn"
		}

		groupSearchReq := goldap.SearchRequest{
			BaseDN:       groupSearchBase,
			Scope:        goldap.ScopeWholeSubtree,
			DerefAliases: goldap.NeverDerefAliases,
			Attributes:   []string{groupIDAttribute},
			Filter:       filter,
		}

		groupSearchResult, err := server.Connection.Search(&groupSearchReq)
		if err != nil {
			return nil, err
		}

		if len(groupSearchResult.Entries) > 0 {
			for _, group := range groupSearchResult.Entries {
				memberOf = append(
					memberOf,
					getAttribute(groupIDAttribute, group),
				)
			}
		}
	}

	return memberOf, nil
}

// serializeUsers serializes the users
// from LDAP result to ExternalInfo struct
func (server *Server) serializeUsers(
	entries []*goldap.Entry,
) ([]*models.User, error) {
	var serialized []*models.User

	for _, user := range entries {
		extUser, err := server.buildGoldenUser(user)
		if err != nil {
			return nil, err
		}

		serialized = append(serialized, extUser)
	}

	return serialized, nil
}

// getMemberOf finds memberOf property or request it
func (server *Server) getMemberOf(result *goldap.Entry) (
	[]string, error,
) {
	if server.Config.GroupSearchFilter == "" {
		memberOf := getArrayAttribute(server.Config.Attr.MemberOf, result)

		return memberOf, nil
	}

	memberOf, err := server.requestMemberOf(result)
	if err != nil {
		return nil, err
	}

	return memberOf, nil
}
