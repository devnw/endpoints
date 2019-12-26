package endpoints

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/pkg/errors"
	auth "gopkg.in/korylprince/go-ad-auth.v2"
	"gopkg.in/ldap.v3"
	"github.com/nortonlifelock/database/dal"
	"github.com/nortonlifelock/domain"
)

type customClaims struct {
	jwt.StandardClaims
	Permissions *tokenPermission
}

var (
	// SigningKey is generated by the listener main method. It is used in the JWT for hash verification
	SigningKey string
)

func authenticate(listenerConfig ADConfig, user domain.User, password string, orgID string) (token string, groups []string, err error) {
	if len(SigningKey) > 0 {
		if len(password) > 0 && len(sord(user.Username())) > 0 {
			token, groups, err = authenticateAgainstAD(listenerConfig, user, password, orgID)
			if err != nil {
				err = fmt.Errorf("failed login for [%s]: "+err.Error(), sord(user.Username()))
			}
		} else {
			err = errors.New("Both a username and password cannot be empty")
		}
	} else {
		err = errors.New("signing key not initialized for authentication")
	}

	return token, groups, err
}

func authenticateAgainstAD(listenerConfig ADConfig, user domain.User, password string, orgID string) (sessionToken string, groups []string, err error) {
	if len(listenerConfig.Servers) > 0 {
		for _, server := range listenerConfig.Servers {
			sessionToken, groups, err = connectADServer(listenerConfig, server, user, password, orgID)
			if err == nil {
				return sessionToken, groups, err
			} else if strings.Contains(err.Error(), "Invalid credentials") {
				err = errors.New("Invalid credentials")
				break
			}
		}

		if err != nil {
			if strings.Index(err.Error(), "Can't contact LDAP server") > 0 {
				err = errors.New("could not connect to LDAP server")
			}
		}
	} else {
		err = fmt.Errorf("no AD servers found in the listener config")
	}

	return "", groups, err
}

func connectADServer(listenerConfig ADConfig, ldapServer string, user domain.User, password string, orgID string) (jwt string, groups []string, err error) {
	groups, err = authenticateAndGetUserGroupsFromAD(
		listenerConfig,
		ldapServer,
		sord(user.Username()),
		password,
		user.FirstName(),
		user.LastName(),
	)
	if err == nil {
		jwt, err = generateJWT(sord(user.Username()), orgID)
	}

	return jwt, groups, err
}

// e.g. searchString should have a %s in the place of where the username is inserted, for example: (sAMAccountName=%s)
func authenticateAndGetUserGroupsFromAD(listenerConfig ADConfig, server string, user string, password string, first string, last string) (groups []string, err error) {
	config := &auth.Config{
		Server:   server,
		Port:     listenerConfig.ADLdapTLSPort,
		BaseDN:   listenerConfig.ADBaseDN,
		Security: auth.SecurityTLS,
	}

	var ldapConn *ldap.Conn
	ldapConn, err = ldap.DialTLS(
		"tcp",
		fmt.Sprintf("%s:%d", server, listenerConfig.ADLdapTLSPort),
		&tls.Config{
			ServerName:         server,
			InsecureSkipVerify: listenerConfig.ADSkipTLSVerify,
		},
	)
	if err == nil {
		adConn := &auth.Conn{Conn: ldapConn, Config: config}

		var ok bool
		ok, err = adConn.Bind(fmt.Sprintf("CN=%s %s,%s", first, last, listenerConfig.ADBaseDN), password)
		if err == nil {
			if ok {
				//var entries []*ldap.Entry
				//entries, err = adConn.Search(fmt.Sprintf(listenerConfig.ADSearchString, user), []string{listenerConfig.ADMemberOfAttribute}, 0)
				//if err == nil {
				//
				//	groups = make([]string, 0)
				//
				//	for _, entry := range entries {
				//		for _, attribute := range entry.Attributes {
				//			if attribute.Name == listenerConfig.ADMemberOfAttribute {
				//				for _, value := range attribute.Values {
				//					groups = append(groups, value)
				//				}
				//			}
				//		}
				//	}
				//
				//	if len(groups) == 0 {
				//		err = fmt.Errorf("could not find any groups %s was a member of", sord(user.Username()))
				//	}
				//} else {
				//	err = fmt.Errorf("error while looking up user groups - %s", err.Error())
				//}
			} else {
				err = fmt.Errorf("invalid credentials")
			}
		} else {
			err = fmt.Errorf("error while contacting AD, likely invalid credentials - %s", err.Error())
		}
	} else {
		err = fmt.Errorf("error while contacting AD - %s", err.Error())
	}

	return groups, err
}

func generateJWT(username string, orgID string) (webToken string, err error) {
	mySigningKey := []byte(SigningKey)
	var permissions domain.Permission
	var user domain.User

	user, err = Ms.GetUserByUsername(username)
	if err == nil {
		if user != nil {
			if len(orgID) > 0 {
				permissions, err = Ms.GetPermissionByUserOrgID(user.ID(), orgID)
			} else {
				permissions, err = Ms.GetPermissionOfLeafOrgByUserID(user.ID())
			}

			if err == nil {
				if permissions != nil {
					tokenPerm := &tokenPermission{permissions.(*dal.Permission)}

					claims := customClaims{
						jwt.StandardClaims{
							ExpiresAt: time.Now().Add(time.Hour).Unix(),
							Issuer:    "PDE",
						},
						tokenPerm,
					}

					token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
					webToken, err = token.SignedString(mySigningKey)
				} else {
					err = errors.Errorf("could not find permissions for user %s", username)
				}
			}
		} else {
			err = errors.Errorf("could not find user in database %s", username)
		}
	}

	return webToken, err
}

func checkJWT(tokenString string) (claims *customClaims, err error) {
	var token *jwt.Token

	token, err = jwt.ParseWithClaims(tokenString, &customClaims{}, func(token *jwt.Token) (interface{}, error) {
		return []byte(SigningKey), nil
	})

	if err == nil {
		if token != nil {
			if len(token.Raw) > 0 {
				var isClaim bool
				claims, isClaim = token.Claims.(*customClaims)
				if isClaim && token.Valid {
					jwtFields := strings.Split(token.Raw, ".")
					if len(jwtFields) == 3 {
						var bodyInB64 = jwtFields[1]
						trailingBytes := len(bodyInB64) % 4
						if trailingBytes != 0 { //jwt doesn't terminate their b64 strings
							bodyInB64 += strings.Repeat("=", 4-trailingBytes)
						}

						var decodedBody []byte
						decodedBody, err = base64.StdEncoding.DecodeString(bodyInB64)
						if err == nil {
							var trimBody = string(decodedBody)
							var prefix = "\"Permissions\":"
							var indexAfterPrefix = strings.Index(trimBody, prefix) + len(prefix)
							trimBody = trimBody[indexAfterPrefix : len(trimBody)-1]
							err = claims.Permissions.UnmarshalJSON([]byte(trimBody))
						}

					} else {
						err = errors.New("improperly formatted JWT")
					}

				} else {
					claims = nil
					err = errors.New("Non-claim based JWT given")
				}

			} else {
				err = errors.New("empty JWT passed")
			}
		} else {
			err = errors.New("could not process JWT")
		}
	} else if validationErr, isVE := err.(*jwt.ValidationError); isVE {
		if validationErr.Errors&jwt.ValidationErrorMalformed != 0 {
			err = errors.New("Non-token passed")
		} else if validationErr.Errors&(jwt.ValidationErrorExpired|jwt.ValidationErrorNotValidYet) != 0 {
			err = errors.New("Token is either expired or not active yet")
		} else {
			err = errors.New("Could not handle token")
		}
	}

	return claims, err
}
