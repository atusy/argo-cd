package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dgrijalva/jwt-go/v4"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/argoproj/argo-cd/v2/common"
	"github.com/argoproj/argo-cd/v2/pkg/apiclient"
	"github.com/argoproj/argo-cd/v2/pkg/apiclient/session"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	apps "github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned/fake"
	servercache "github.com/argoproj/argo-cd/v2/server/cache"
	"github.com/argoproj/argo-cd/v2/server/rbacpolicy"
	"github.com/argoproj/argo-cd/v2/test"
	"github.com/argoproj/argo-cd/v2/util/assets"
	cacheutil "github.com/argoproj/argo-cd/v2/util/cache"
	appstatecache "github.com/argoproj/argo-cd/v2/util/cache/appstate"
	"github.com/argoproj/argo-cd/v2/util/rbac"
)

func fakeServer() (*ArgoCDServer, func()) {
	cm := test.NewFakeConfigMap()
	secret := test.NewFakeSecret()
	kubeclientset := fake.NewSimpleClientset(cm, secret)
	appClientSet := apps.NewSimpleClientset()
	redis, closer := test.NewInMemoryRedis()

	argoCDOpts := ArgoCDServerOpts{
		Namespace:     test.FakeArgoCDNamespace,
		KubeClientset: kubeclientset,
		AppClientset:  appClientSet,
		Insecure:      true,
		DisableAuth:   true,
		XFrameOptions: "sameorigin",
		Cache: servercache.NewCache(
			appstatecache.NewCache(
				cacheutil.NewCache(cacheutil.NewInMemoryCache(1*time.Hour)),
				1*time.Minute,
			),
			1*time.Minute,
			1*time.Minute,
			1*time.Minute,
		),
		RedisClient: redis,
	}
	return NewServer(context.Background(), argoCDOpts), closer
}

func TestEnforceProjectToken(t *testing.T) {
	projectName := "testProj"
	roleName := "testRole"
	subFormat := "proj:%s:%s"
	policyTemplate := "p, %s, applications, get, %s/%s, %s"
	defaultObject := "*"
	defaultEffect := "allow"
	defaultTestObject := fmt.Sprintf("%s/%s", projectName, "test")
	defaultIssuedAt := int64(1)
	defaultSub := fmt.Sprintf(subFormat, projectName, roleName)
	defaultPolicy := fmt.Sprintf(policyTemplate, defaultSub, projectName, defaultObject, defaultEffect)
	defaultId := "testId"

	role := v1alpha1.ProjectRole{Name: roleName, Policies: []string{defaultPolicy}, JWTTokens: []v1alpha1.JWTToken{{IssuedAt: defaultIssuedAt}, {ID: defaultId}}}

	jwtTokenByRole := make(map[string]v1alpha1.JWTTokens)
	jwtTokenByRole[roleName] = v1alpha1.JWTTokens{Items: []v1alpha1.JWTToken{{IssuedAt: defaultIssuedAt}, {ID: defaultId}}}

	existingProj := v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: test.FakeArgoCDNamespace},
		Spec: v1alpha1.AppProjectSpec{
			Roles: []v1alpha1.ProjectRole{role},
		},
		Status: v1alpha1.AppProjectStatus{JWTTokensByRole: jwtTokenByRole},
	}
	cm := test.NewFakeConfigMap()
	secret := test.NewFakeSecret()
	kubeclientset := fake.NewSimpleClientset(cm, secret)

	t.Run("TestEnforceProjectTokenSuccessful", func(t *testing.T) {
		s := NewServer(context.Background(), ArgoCDServerOpts{Namespace: test.FakeArgoCDNamespace, KubeClientset: kubeclientset, AppClientset: apps.NewSimpleClientset(&existingProj)})
		cancel := test.StartInformer(s.projInformer)
		defer cancel()
		claims := jwt.MapClaims{"sub": defaultSub, "iat": defaultIssuedAt}
		assert.True(t, s.enf.Enforce(claims, "projects", "get", existingProj.ObjectMeta.Name))
		assert.True(t, s.enf.Enforce(claims, "applications", "get", defaultTestObject))
	})

	t.Run("TestEnforceProjectTokenWithDiffCreateAtFailure", func(t *testing.T) {
		s := NewServer(context.Background(), ArgoCDServerOpts{Namespace: test.FakeArgoCDNamespace, KubeClientset: kubeclientset, AppClientset: apps.NewSimpleClientset(&existingProj)})
		diffCreateAt := defaultIssuedAt + 1
		claims := jwt.MapClaims{"sub": defaultSub, "iat": diffCreateAt}
		assert.False(t, s.enf.Enforce(claims, "applications", "get", defaultTestObject))
	})

	t.Run("TestEnforceProjectTokenIncorrectSubFormatFailure", func(t *testing.T) {
		s := NewServer(context.Background(), ArgoCDServerOpts{Namespace: test.FakeArgoCDNamespace, KubeClientset: kubeclientset, AppClientset: apps.NewSimpleClientset(&existingProj)})
		invalidSub := "proj:test"
		claims := jwt.MapClaims{"sub": invalidSub, "iat": defaultIssuedAt}
		assert.False(t, s.enf.Enforce(claims, "applications", "get", defaultTestObject))
	})

	t.Run("TestEnforceProjectTokenNoTokenFailure", func(t *testing.T) {
		s := NewServer(context.Background(), ArgoCDServerOpts{Namespace: test.FakeArgoCDNamespace, KubeClientset: kubeclientset, AppClientset: apps.NewSimpleClientset(&existingProj)})
		nonExistentToken := "fake-token"
		invalidSub := fmt.Sprintf(subFormat, projectName, nonExistentToken)
		claims := jwt.MapClaims{"sub": invalidSub, "iat": defaultIssuedAt}
		assert.False(t, s.enf.Enforce(claims, "applications", "get", defaultTestObject))
	})

	t.Run("TestEnforceProjectTokenNotJWTTokenFailure", func(t *testing.T) {
		proj := existingProj.DeepCopy()
		proj.Spec.Roles[0].JWTTokens = nil
		s := NewServer(context.Background(), ArgoCDServerOpts{Namespace: test.FakeArgoCDNamespace, KubeClientset: kubeclientset, AppClientset: apps.NewSimpleClientset(proj)})
		claims := jwt.MapClaims{"sub": defaultSub, "iat": defaultIssuedAt}
		assert.False(t, s.enf.Enforce(claims, "applications", "get", defaultTestObject))
	})

	t.Run("TestEnforceProjectTokenExplicitDeny", func(t *testing.T) {
		denyApp := "testDenyApp"
		allowPolicy := fmt.Sprintf(policyTemplate, defaultSub, projectName, defaultObject, defaultEffect)
		denyPolicy := fmt.Sprintf(policyTemplate, defaultSub, projectName, denyApp, "deny")
		role := v1alpha1.ProjectRole{Name: roleName, Policies: []string{allowPolicy, denyPolicy}, JWTTokens: []v1alpha1.JWTToken{{IssuedAt: defaultIssuedAt}}}
		proj := existingProj.DeepCopy()
		proj.Spec.Roles[0] = role

		s := NewServer(context.Background(), ArgoCDServerOpts{Namespace: test.FakeArgoCDNamespace, KubeClientset: kubeclientset, AppClientset: apps.NewSimpleClientset(proj)})
		cancel := test.StartInformer(s.projInformer)
		defer cancel()
		claims := jwt.MapClaims{"sub": defaultSub, "iat": defaultIssuedAt}
		allowedObject := fmt.Sprintf("%s/%s", projectName, "test")
		denyObject := fmt.Sprintf("%s/%s", projectName, denyApp)
		assert.True(t, s.enf.Enforce(claims, "applications", "get", allowedObject))
		assert.False(t, s.enf.Enforce(claims, "applications", "get", denyObject))
	})

	t.Run("TestEnforceProjectTokenWithIdSuccessful", func(t *testing.T) {
		s := NewServer(context.Background(), ArgoCDServerOpts{Namespace: test.FakeArgoCDNamespace, KubeClientset: kubeclientset, AppClientset: apps.NewSimpleClientset(&existingProj)})
		cancel := test.StartInformer(s.projInformer)
		defer cancel()
		claims := jwt.MapClaims{"sub": defaultSub, "jti": defaultId}
		assert.True(t, s.enf.Enforce(claims, "projects", "get", existingProj.ObjectMeta.Name))
		assert.True(t, s.enf.Enforce(claims, "applications", "get", defaultTestObject))
	})

	t.Run("TestEnforceProjectTokenWithInvalidIdFailure", func(t *testing.T) {
		s := NewServer(context.Background(), ArgoCDServerOpts{Namespace: test.FakeArgoCDNamespace, KubeClientset: kubeclientset, AppClientset: apps.NewSimpleClientset(&existingProj)})
		invalidId := "invalidId"
		claims := jwt.MapClaims{"sub": defaultSub, "jti": defaultId}
		res := s.enf.Enforce(claims, "applications", "get", invalidId)
		assert.False(t, res)
	})

}

func TestEnforceClaims(t *testing.T) {
	kubeclientset := fake.NewSimpleClientset(test.NewFakeConfigMap())
	enf := rbac.NewEnforcer(kubeclientset, test.FakeArgoCDNamespace, common.ArgoCDConfigMapName, nil)
	_ = enf.SetBuiltinPolicy(assets.BuiltinPolicyCSV)
	rbacEnf := rbacpolicy.NewRBACPolicyEnforcer(enf, test.NewFakeProjLister())
	enf.SetClaimsEnforcerFunc(rbacEnf.EnforceClaims)
	policy := `
g, org2:team2, role:admin
g, bob, role:admin
`
	_ = enf.SetUserPolicy(policy)
	allowed := []jwt.Claims{
		jwt.MapClaims{"groups": []string{"org1:team1", "org2:team2"}},
		jwt.StandardClaims{Subject: "admin"},
	}
	for _, c := range allowed {
		if !assert.True(t, enf.Enforce(c, "applications", "delete", "foo/obj")) {
			log.Errorf("%v: expected true, got false", c)
		}
	}

	disallowed := []jwt.Claims{
		jwt.MapClaims{"groups": []string{"org3:team3"}},
		jwt.StandardClaims{Subject: "nobody"},
	}
	for _, c := range disallowed {
		if !assert.False(t, enf.Enforce(c, "applications", "delete", "foo/obj")) {
			log.Errorf("%v: expected true, got false", c)
		}
	}
}

func TestDefaultRoleWithClaims(t *testing.T) {
	kubeclientset := fake.NewSimpleClientset()
	enf := rbac.NewEnforcer(kubeclientset, test.FakeArgoCDNamespace, common.ArgoCDConfigMapName, nil)
	_ = enf.SetBuiltinPolicy(assets.BuiltinPolicyCSV)
	rbacEnf := rbacpolicy.NewRBACPolicyEnforcer(enf, test.NewFakeProjLister())
	enf.SetClaimsEnforcerFunc(rbacEnf.EnforceClaims)
	claims := jwt.MapClaims{"groups": []string{"org1:team1", "org2:team2"}}

	assert.False(t, enf.Enforce(claims, "applications", "get", "foo/bar"))
	// after setting the default role to be the read-only role, this should now pass
	enf.SetDefaultRole("role:readonly")
	assert.True(t, enf.Enforce(claims, "applications", "get", "foo/bar"))
}

func TestEnforceNilClaims(t *testing.T) {
	kubeclientset := fake.NewSimpleClientset(test.NewFakeConfigMap())
	enf := rbac.NewEnforcer(kubeclientset, test.FakeArgoCDNamespace, common.ArgoCDConfigMapName, nil)
	_ = enf.SetBuiltinPolicy(assets.BuiltinPolicyCSV)
	rbacEnf := rbacpolicy.NewRBACPolicyEnforcer(enf, test.NewFakeProjLister())
	enf.SetClaimsEnforcerFunc(rbacEnf.EnforceClaims)
	assert.False(t, enf.Enforce(nil, "applications", "get", "foo/obj"))
	enf.SetDefaultRole("role:readonly")
	assert.True(t, enf.Enforce(nil, "applications", "get", "foo/obj"))
}

func TestInitializingExistingDefaultProject(t *testing.T) {
	cm := test.NewFakeConfigMap()
	secret := test.NewFakeSecret()
	kubeclientset := fake.NewSimpleClientset(cm, secret)
	defaultProj := &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAppProjectName, Namespace: test.FakeArgoCDNamespace},
		Spec:       v1alpha1.AppProjectSpec{},
	}
	appClientSet := apps.NewSimpleClientset(defaultProj)

	argoCDOpts := ArgoCDServerOpts{
		Namespace:     test.FakeArgoCDNamespace,
		KubeClientset: kubeclientset,
		AppClientset:  appClientSet,
	}

	argocd := NewServer(context.Background(), argoCDOpts)
	assert.NotNil(t, argocd)

	proj, err := appClientSet.ArgoprojV1alpha1().AppProjects(test.FakeArgoCDNamespace).Get(context.Background(), v1alpha1.DefaultAppProjectName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.NotNil(t, proj)
	assert.Equal(t, proj.Name, v1alpha1.DefaultAppProjectName)
}

func TestInitializingNotExistingDefaultProject(t *testing.T) {
	cm := test.NewFakeConfigMap()
	secret := test.NewFakeSecret()
	kubeclientset := fake.NewSimpleClientset(cm, secret)
	appClientSet := apps.NewSimpleClientset()

	argoCDOpts := ArgoCDServerOpts{
		Namespace:     test.FakeArgoCDNamespace,
		KubeClientset: kubeclientset,
		AppClientset:  appClientSet,
	}

	argocd := NewServer(context.Background(), argoCDOpts)
	assert.NotNil(t, argocd)

	proj, err := appClientSet.ArgoprojV1alpha1().AppProjects(test.FakeArgoCDNamespace).Get(context.Background(), v1alpha1.DefaultAppProjectName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.NotNil(t, proj)
	assert.Equal(t, proj.Name, v1alpha1.DefaultAppProjectName)
}

func TestEnforceProjectGroups(t *testing.T) {
	projectName := "testProj"
	roleName := "testRole"
	subFormat := "proj:%s:%s"
	policyTemplate := "p, %s, applications, get, %s/%s, %s"
	groupName := "my-org:my-team"

	defaultObject := "*"
	defaultEffect := "allow"
	defaultTestObject := fmt.Sprintf("%s/%s", projectName, "test")
	defaultIssuedAt := int64(1)
	defaultSub := fmt.Sprintf(subFormat, projectName, roleName)
	defaultPolicy := fmt.Sprintf(policyTemplate, defaultSub, projectName, defaultObject, defaultEffect)

	existingProj := v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      projectName,
			Namespace: test.FakeArgoCDNamespace,
		},
		Spec: v1alpha1.AppProjectSpec{
			Roles: []v1alpha1.ProjectRole{
				{
					Name:     roleName,
					Policies: []string{defaultPolicy},
					Groups: []string{
						groupName,
					},
				},
			},
		},
	}
	kubeclientset := fake.NewSimpleClientset(test.NewFakeConfigMap(), test.NewFakeSecret())
	s := NewServer(context.Background(), ArgoCDServerOpts{Namespace: test.FakeArgoCDNamespace, KubeClientset: kubeclientset, AppClientset: apps.NewSimpleClientset(&existingProj)})
	cancel := test.StartInformer(s.projInformer)
	defer cancel()
	claims := jwt.MapClaims{
		"iat":    defaultIssuedAt,
		"groups": []string{groupName},
	}
	assert.True(t, s.enf.Enforce(claims, "projects", "get", existingProj.ObjectMeta.Name))
	assert.True(t, s.enf.Enforce(claims, "applications", "get", defaultTestObject))
	assert.False(t, s.enf.Enforce(claims, "clusters", "get", "test"))

	// now remove the group and make sure it fails
	log.Println(existingProj.ProjectPoliciesString())
	existingProj.Spec.Roles[0].Groups = nil
	log.Println(existingProj.ProjectPoliciesString())
	_, _ = s.AppClientset.ArgoprojV1alpha1().AppProjects(test.FakeArgoCDNamespace).Update(context.Background(), &existingProj, metav1.UpdateOptions{})
	time.Sleep(100 * time.Millisecond) // this lets the informer get synced
	assert.False(t, s.enf.Enforce(claims, "projects", "get", existingProj.ObjectMeta.Name))
	assert.False(t, s.enf.Enforce(claims, "applications", "get", defaultTestObject))
	assert.False(t, s.enf.Enforce(claims, "clusters", "get", "test"))
}

func TestRevokedToken(t *testing.T) {
	projectName := "testProj"
	roleName := "testRole"
	subFormat := "proj:%s:%s"
	policyTemplate := "p, %s, applications, get, %s/%s, %s"
	defaultObject := "*"
	defaultEffect := "allow"
	defaultTestObject := fmt.Sprintf("%s/%s", projectName, "test")
	defaultIssuedAt := int64(1)
	defaultSub := fmt.Sprintf(subFormat, projectName, roleName)
	defaultPolicy := fmt.Sprintf(policyTemplate, defaultSub, projectName, defaultObject, defaultEffect)
	kubeclientset := fake.NewSimpleClientset(test.NewFakeConfigMap(), test.NewFakeSecret())

	jwtTokenByRole := make(map[string]v1alpha1.JWTTokens)
	jwtTokenByRole[roleName] = v1alpha1.JWTTokens{Items: []v1alpha1.JWTToken{{IssuedAt: defaultIssuedAt}}}

	existingProj := v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      projectName,
			Namespace: test.FakeArgoCDNamespace,
		},
		Spec: v1alpha1.AppProjectSpec{
			Roles: []v1alpha1.ProjectRole{
				{
					Name:     roleName,
					Policies: []string{defaultPolicy},
					JWTTokens: []v1alpha1.JWTToken{
						{
							IssuedAt: defaultIssuedAt,
						},
					},
				},
			},
		},
		Status: v1alpha1.AppProjectStatus{
			JWTTokensByRole: jwtTokenByRole,
		},
	}

	s := NewServer(context.Background(), ArgoCDServerOpts{Namespace: test.FakeArgoCDNamespace, KubeClientset: kubeclientset, AppClientset: apps.NewSimpleClientset(&existingProj)})
	cancel := test.StartInformer(s.projInformer)
	defer cancel()
	claims := jwt.MapClaims{"sub": defaultSub, "iat": defaultIssuedAt}
	assert.True(t, s.enf.Enforce(claims, "projects", "get", existingProj.ObjectMeta.Name))
	assert.True(t, s.enf.Enforce(claims, "applications", "get", defaultTestObject))
}

func TestCertsAreNotGeneratedInInsecureMode(t *testing.T) {
	s, closer := fakeServer()
	defer closer()
	assert.True(t, s.Insecure)
	assert.Nil(t, s.settings.Certificate)
}

func TestAuthenticate(t *testing.T) {
	type testData struct {
		test             string
		user             string
		errorMsg         string
		anonymousEnabled bool
	}
	var tests = []testData{
		{
			test:             "TestNoSessionAnonymousDisabled",
			errorMsg:         "no session information",
			anonymousEnabled: false,
		},
		{
			test:             "TestSessionPresent",
			user:             "admin:login",
			anonymousEnabled: false,
		},
		{
			test:             "TestSessionNotPresentAnonymousEnabled",
			anonymousEnabled: true,
		},
	}

	for _, testData := range tests {
		t.Run(testData.test, func(t *testing.T) {
			cm := test.NewFakeConfigMap()
			if testData.anonymousEnabled {
				cm.Data["users.anonymous.enabled"] = "true"
			}
			secret := test.NewFakeSecret()
			kubeclientset := fake.NewSimpleClientset(cm, secret)
			appClientSet := apps.NewSimpleClientset()
			argoCDOpts := ArgoCDServerOpts{
				Namespace:     test.FakeArgoCDNamespace,
				KubeClientset: kubeclientset,
				AppClientset:  appClientSet,
			}
			argocd := NewServer(context.Background(), argoCDOpts)
			ctx := context.Background()
			if testData.user != "" {
				token, err := argocd.sessionMgr.Create(testData.user, 0, "abc")
				assert.NoError(t, err)
				ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs(apiclient.MetaDataTokenKey, token))
			}

			_, err := argocd.Authenticate(ctx)
			if testData.errorMsg != "" {
				assert.Errorf(t, err, testData.errorMsg)
			} else {
				assert.NoError(t, err)
			}

		})
	}
}

func dexMockHandler(t *testing.T, url string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.RequestURI {
		case "/api/dex/.well-known/openid-configuration":
			_, err := io.WriteString(w, fmt.Sprintf(`
{
  "issuer": "%[1]s/api/dex",
  "authorization_endpoint": "%[1]s/api/dex/auth",
  "token_endpoint": "%[1]s/api/dex/token",
  "jwks_uri": "%[1]s/api/dex/keys",
  "userinfo_endpoint": "%[1]s/api/dex/userinfo",
  "device_authorization_endpoint": "%[1]s/api/dex/device/code",
  "grant_types_supported": [
    "authorization_code",
    "refresh_token",
    "urn:ietf:params:oauth:grant-type:device_code"
  ],
  "response_types_supported": [
    "code"
  ],
  "subject_types_supported": [
    "public"
  ],
  "id_token_signing_alg_values_supported": [
    "RS256", "HS256"
  ],
  "code_challenge_methods_supported": [
    "S256",
    "plain"
  ],
  "scopes_supported": [
    "openid",
    "email",
    "groups",
    "profile",
    "offline_access"
  ],
  "token_endpoint_auth_methods_supported": [
    "client_secret_basic",
    "client_secret_post"
  ],
  "claims_supported": [
    "iss",
    "sub",
    "aud",
    "iat",
    "exp",
    "email",
    "email_verified",
    "locale",
    "name",
    "preferred_username",
    "at_hash"
  ]
}`, url))
			if err != nil {
				t.Fail()
			}
		default:
			w.WriteHeader(404)
		}
	}
}

func getTestServer(t *testing.T, anonymousEnabled bool, withFakeSSO bool) (argocd *ArgoCDServer, dexURL string) {
	cm := test.NewFakeConfigMap()
	if anonymousEnabled {
		cm.Data["users.anonymous.enabled"] = "true"
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		return // Start with a placeholder. We need the server URL before setting up the real handler.
	}))
	ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dexMockHandler(t, ts.URL)(w, r)
	})
	if withFakeSSO {
		cm.Data["url"] = ts.URL
		cm.Data["dex.config"] = `
connectors:
  # OIDC
  - type: OIDC
    id: oidc
    name: OIDC
    config:
    issuer: https://auth.example.gom
    clientID: test-client
    clientSecret: $dex.oidc.clientSecret`
	}
	secret := test.NewFakeSecret()
	kubeclientset := fake.NewSimpleClientset(cm, secret)
	appClientSet := apps.NewSimpleClientset()
	argoCDOpts := ArgoCDServerOpts{
		Namespace:     test.FakeArgoCDNamespace,
		KubeClientset: kubeclientset,
		AppClientset:  appClientSet,
	}
	if withFakeSSO {
		argoCDOpts.DexServerAddr = ts.URL
	}
	argocd = NewServer(context.Background(), argoCDOpts)
	return argocd, ts.URL
}

func TestAuthenticate_3rd_party_JWTs(t *testing.T) {
	type testData struct {
		test                  string
		anonymousEnabled      bool
		claims                jwt.StandardClaims
		expectedErrorContains string
		expectedClaims        interface{}
	}
	var tests = []testData{
		{
			test:                  "anonymous disabled, no audience",
			anonymousEnabled:      false,
			claims:                jwt.StandardClaims{},
			expectedErrorContains: "no audience found in the token",
			expectedClaims:        nil,
		},
		{
			test:                  "anonymous enabled, no audience",
			anonymousEnabled:      true,
			claims:                jwt.StandardClaims{},
			expectedErrorContains: "",
			expectedClaims:        "",
		},
		{
			test:                  "anonymous disabled, unexpired token, admin claim",
			anonymousEnabled:      false,
			claims:                jwt.StandardClaims{Audience: jwt.ClaimStrings{"test-client"}, Subject: "admin", ExpiresAt: jwt.NewTime(float64(time.Now().Add(time.Hour * 24).Unix()))},
			expectedErrorContains: "id token signed with unsupported algorithm",
			expectedClaims:        nil,
		},
		{
			test:                  "anonymous enabled, unexpired token, admin claim",
			anonymousEnabled:      true,
			claims:                jwt.StandardClaims{Audience: jwt.ClaimStrings{"test-client"}, Subject: "admin", ExpiresAt: jwt.NewTime(float64(time.Now().Add(time.Hour * 24).Unix()))},
			expectedErrorContains: "",
			expectedClaims:        "",
		},
		{
			test:                  "anonymous disabled, expired token, admin claim",
			anonymousEnabled:      false,
			claims:                jwt.StandardClaims{Audience: jwt.ClaimStrings{"test-client"}, Subject: "admin", ExpiresAt: jwt.NewTime(float64(time.Now().Unix()))},
			expectedErrorContains: "token is expired",
			expectedClaims:        jwt.StandardClaims{Issuer: "sso"},
		},
		{
			test:                  "anonymous enabled, expired token, admin claim",
			anonymousEnabled:      true,
			claims:                jwt.StandardClaims{Audience: jwt.ClaimStrings{"test-client"}, Subject: "admin", ExpiresAt: jwt.NewTime(float64(time.Now().Unix()))},
			expectedErrorContains: "",
			expectedClaims:        "",
		},
	}

	for _, testData := range tests {
		testDataCopy := testData

		t.Run(testDataCopy.test, func(t *testing.T) {
			t.Parallel()

			argocd, dexURL := getTestServer(t, testDataCopy.anonymousEnabled, true)
			ctx := context.Background()
			testDataCopy.claims.Issuer = fmt.Sprintf("%s/api/dex", dexURL)
			token := jwt.NewWithClaims(jwt.SigningMethodHS256, testDataCopy.claims)
			tokenString, err := token.SignedString([]byte("key"))
			require.NoError(t, err)
			ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs(apiclient.MetaDataTokenKey, tokenString))

			ctx, err = argocd.Authenticate(ctx)
			claims := ctx.Value("claims")
			if testDataCopy.expectedClaims == nil {
				assert.Nil(t, claims)
			} else {
				assert.Equal(t, testDataCopy.expectedClaims, claims)
			}
			if testDataCopy.expectedErrorContains != "" {
				assert.Error(t, err, "Authenticate should have thrown an error and blocked the request")
				assert.Contains(t, err.Error(), testDataCopy.expectedErrorContains)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestAuthenticate_no_request_metadata(t *testing.T) {
	type testData struct {
		test                  string
		anonymousEnabled      bool
		expectedErrorContains string
		expectedClaims        interface{}
	}
	var tests = []testData{
		{
			test:                  "anonymous disabled",
			anonymousEnabled:      false,
			expectedErrorContains: "no session information",
			expectedClaims:        nil,
		},
		{
			test:                  "anonymous enabled",
			anonymousEnabled:      true,
			expectedErrorContains: "",
			expectedClaims:        "",
		},
	}

	for _, testData := range tests {
		testDataCopy := testData

		t.Run(testDataCopy.test, func(t *testing.T) {
			t.Parallel()

			argocd, _ := getTestServer(t, testDataCopy.anonymousEnabled, true)
			ctx := context.Background()

			ctx, err := argocd.Authenticate(ctx)
			claims := ctx.Value("claims")
			assert.Equal(t, testDataCopy.expectedClaims, claims)
			if testDataCopy.expectedErrorContains != "" {
				assert.Error(t, err, "Authenticate should have thrown an error and blocked the request")
				assert.Contains(t, err.Error(), testDataCopy.expectedErrorContains)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestAuthenticate_no_SSO(t *testing.T) {
	type testData struct {
		test                 string
		anonymousEnabled     bool
		expectedErrorMessage string
		expectedClaims       interface{}
	}
	var tests = []testData{
		{
			test:                 "anonymous disabled",
			anonymousEnabled:     false,
			expectedErrorMessage: "SSO is not configured",
			expectedClaims:       nil,
		},
		{
			test:                 "anonymous enabled",
			anonymousEnabled:     true,
			expectedErrorMessage: "",
			expectedClaims:       "",
		},
	}

	for _, testData := range tests {
		testDataCopy := testData

		t.Run(testDataCopy.test, func(t *testing.T) {
			t.Parallel()

			argocd, dexURL := getTestServer(t, testDataCopy.anonymousEnabled, false)
			ctx := context.Background()
			token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.StandardClaims{Issuer: fmt.Sprintf("%s/api/dex", dexURL)})
			tokenString, err := token.SignedString([]byte("key"))
			require.NoError(t, err)
			ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs(apiclient.MetaDataTokenKey, tokenString))

			ctx, err = argocd.Authenticate(ctx)
			claims := ctx.Value("claims")
			assert.Equal(t, testDataCopy.expectedClaims, claims)
			if testDataCopy.expectedErrorMessage != "" {
				assert.Error(t, err, "Authenticate should have thrown an error and blocked the request")
				assert.Contains(t, err.Error(), testDataCopy.expectedErrorMessage)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestAuthenticate_bad_request_metadata(t *testing.T) {
	type testData struct {
		test                 string
		anonymousEnabled     bool
		metadata             metadata.MD
		expectedErrorMessage string
		expectedClaims       interface{}
	}
	var tests = []testData{
		{
			test:                 "anonymous disabled, empty metadata",
			anonymousEnabled:     false,
			metadata:             metadata.MD{},
			expectedErrorMessage: "no session information",
			expectedClaims:       nil,
		},
		{
			test:                 "anonymous enabled, empty metadata",
			anonymousEnabled:     true,
			metadata:             metadata.MD{},
			expectedErrorMessage: "",
			expectedClaims:       "",
		},
		{
			test:                 "anonymous disabled, empty tokens",
			anonymousEnabled:     false,
			metadata:             metadata.MD{apiclient.MetaDataTokenKey: []string{}},
			expectedErrorMessage: "no session information",
			expectedClaims:       nil,
		},
		{
			test:                 "anonymous enabled, empty tokens",
			anonymousEnabled:     true,
			metadata:             metadata.MD{apiclient.MetaDataTokenKey: []string{}},
			expectedErrorMessage: "",
			expectedClaims:       "",
		},
		{
			test:                 "anonymous disabled, bad tokens",
			anonymousEnabled:     false,
			metadata:             metadata.Pairs(apiclient.MetaDataTokenKey, "bad"),
			expectedErrorMessage: "token contains an invalid number of segments",
			expectedClaims:       nil,
		},
		{
			test:                 "anonymous enabled, bad tokens",
			anonymousEnabled:     true,
			metadata:             metadata.Pairs(apiclient.MetaDataTokenKey, "bad"),
			expectedErrorMessage: "",
			expectedClaims:       "",
		},
		{
			test:                 "anonymous disabled, bad auth header",
			anonymousEnabled:     false,
			metadata:             metadata.MD{"authorization": []string{"Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZG1pbiJ9.TGGTTHuuGpEU8WgobXxkrBtW3NiR3dgw5LR-1DEW3BQ"}},
			expectedErrorMessage: "no audience found in the token",
			expectedClaims:       nil,
		},
		{
			test:                 "anonymous enabled, bad auth header",
			anonymousEnabled:     true,
			metadata:             metadata.MD{"authorization": []string{"Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZG1pbiJ9.TGGTTHuuGpEU8WgobXxkrBtW3NiR3dgw5LR-1DEW3BQ"}},
			expectedErrorMessage: "",
			expectedClaims:       "",
		},
		{
			test:                 "anonymous disabled, bad auth cookie",
			anonymousEnabled:     false,
			metadata:             metadata.MD{"grpcgateway-cookie": []string{"argocd.token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZG1pbiJ9.TGGTTHuuGpEU8WgobXxkrBtW3NiR3dgw5LR-1DEW3BQ"}},
			expectedErrorMessage: "no audience found in the token",
			expectedClaims:       nil,
		},
		{
			test:                 "anonymous enabled, bad auth cookie",
			anonymousEnabled:     true,
			metadata:             metadata.MD{"grpcgateway-cookie": []string{"argocd.token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZG1pbiJ9.TGGTTHuuGpEU8WgobXxkrBtW3NiR3dgw5LR-1DEW3BQ"}},
			expectedErrorMessage: "",
			expectedClaims:       "",
		},
	}

	for _, testData := range tests {
		testDataCopy := testData

		t.Run(testDataCopy.test, func(t *testing.T) {
			t.Parallel()

			argocd, _ := getTestServer(t, testDataCopy.anonymousEnabled, true)
			ctx := context.Background()
			ctx = metadata.NewIncomingContext(context.Background(), testDataCopy.metadata)

			ctx, err := argocd.Authenticate(ctx)
			claims := ctx.Value("claims")
			assert.Equal(t, testDataCopy.expectedClaims, claims)
			if testDataCopy.expectedErrorMessage != "" {
				assert.Error(t, err, "Authenticate should have thrown an error and blocked the request")
				assert.Contains(t, err.Error(), testDataCopy.expectedErrorMessage)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func Test_getToken(t *testing.T) {
	token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	t.Run("Empty", func(t *testing.T) {
		assert.Empty(t, getToken(metadata.New(map[string]string{})))
	})
	t.Run("Token", func(t *testing.T) {
		assert.Equal(t, token, getToken(metadata.New(map[string]string{"token": token})))
	})
	t.Run("Authorisation", func(t *testing.T) {
		assert.Empty(t, getToken(metadata.New(map[string]string{"authorization": "Bearer invalid"})))
		assert.Equal(t, token, getToken(metadata.New(map[string]string{"authorization": "Bearer " + token})))
	})
	t.Run("Cookie", func(t *testing.T) {
		assert.Empty(t, getToken(metadata.New(map[string]string{"grpcgateway-cookie": "argocd.token=invalid"})))
		assert.Equal(t, token, getToken(metadata.New(map[string]string{"grpcgateway-cookie": "argocd.token=" + token})))
	})
}

func TestTranslateGrpcCookieHeader(t *testing.T) {
	argoCDOpts := ArgoCDServerOpts{
		Namespace:     test.FakeArgoCDNamespace,
		KubeClientset: fake.NewSimpleClientset(test.NewFakeConfigMap(), test.NewFakeSecret()),
		AppClientset:  apps.NewSimpleClientset(),
	}
	argocd := NewServer(context.Background(), argoCDOpts)

	t.Run("TokenIsNotEmpty", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		err := argocd.translateGrpcCookieHeader(context.Background(), recorder, &session.SessionResponse{
			Token: "xyz",
		})
		assert.NoError(t, err)
		assert.Equal(t, "argocd.token=xyz; path=/; SameSite=lax; httpOnly; Secure", recorder.Result().Header.Get("Set-Cookie"))
		assert.Equal(t, 1, len(recorder.Result().Cookies()))
	})

	t.Run("TokenIsLongerThan4093", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		err := argocd.translateGrpcCookieHeader(context.Background(), recorder, &session.SessionResponse{
			Token: "abc.xyz." + strings.Repeat("x", 4093),
		})
		assert.NoError(t, err)
		assert.Regexp(t, "argocd.token=.*; path=/; SameSite=lax; httpOnly; Secure", recorder.Result().Header.Get("Set-Cookie"))
		assert.Equal(t, 2, len(recorder.Result().Cookies()))
	})

	t.Run("TokenIsEmpty", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		err := argocd.translateGrpcCookieHeader(context.Background(), recorder, &session.SessionResponse{
			Token: "",
		})
		assert.NoError(t, err)
		assert.Equal(t, "", recorder.Result().Header.Get("Set-Cookie"))
	})

}

func TestInitializeDefaultProject_ProjectDoesNotExist(t *testing.T) {
	argoCDOpts := ArgoCDServerOpts{
		Namespace:     test.FakeArgoCDNamespace,
		KubeClientset: fake.NewSimpleClientset(test.NewFakeConfigMap(), test.NewFakeSecret()),
		AppClientset:  apps.NewSimpleClientset(),
	}

	err := initializeDefaultProject(argoCDOpts)
	if !assert.NoError(t, err) {
		return
	}

	proj, err := argoCDOpts.AppClientset.ArgoprojV1alpha1().
		AppProjects(test.FakeArgoCDNamespace).Get(context.Background(), v1alpha1.DefaultAppProjectName, metav1.GetOptions{})

	if !assert.NoError(t, err) {
		return
	}

	assert.Equal(t, proj.Spec, v1alpha1.AppProjectSpec{
		SourceRepos:              []string{"*"},
		Destinations:             []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
		ClusterResourceWhitelist: []metav1.GroupKind{{Group: "*", Kind: "*"}},
	})
}

func TestInitializeDefaultProject_ProjectAlreadyInitialized(t *testing.T) {
	existingDefaultProject := v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v1alpha1.DefaultAppProjectName,
			Namespace: test.FakeArgoCDNamespace,
		},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:  []string{"some repo"},
			Destinations: []v1alpha1.ApplicationDestination{{Server: "some cluster", Namespace: "*"}},
		},
	}

	argoCDOpts := ArgoCDServerOpts{
		Namespace:     test.FakeArgoCDNamespace,
		KubeClientset: fake.NewSimpleClientset(test.NewFakeConfigMap(), test.NewFakeSecret()),
		AppClientset:  apps.NewSimpleClientset(&existingDefaultProject),
	}

	err := initializeDefaultProject(argoCDOpts)
	if !assert.NoError(t, err) {
		return
	}

	proj, err := argoCDOpts.AppClientset.ArgoprojV1alpha1().
		AppProjects(test.FakeArgoCDNamespace).Get(context.Background(), v1alpha1.DefaultAppProjectName, metav1.GetOptions{})

	if !assert.NoError(t, err) {
		return
	}

	assert.Equal(t, proj.Spec, existingDefaultProject.Spec)
}
