@domain:api-gateway @area:access-control @from:proxy/auth
Feature: Guarding the proxy with its own API key
  Lets an operator who exposes chicco beyond localhost require a shared secret, so only callers holding it can spend the pool's provider keys.

  @id:api-gateway.access-control.001 @rule:gateway.access.open-without-key @verified @source:internal/proxy/auth.go#withAuth @test:internal/proxy/proxy_test.go#TestInboundAuth
  Scenario: With no proxy key configured every caller is served
    Given chicco.yaml sets no top-level api_key
    When a caller requests GET /v1/models without an Authorization header
    Then the response status is 200
    And the model list is returned

  @id:api-gateway.access-control.002 @rule:gateway.access.bearer-key-admits @verified @source:internal/proxy/auth.go#bearerMatches @test:internal/proxy/proxy_test.go#TestInboundAuth
  Scenario: A caller presenting the proxy key as a bearer token is served
    Given chicco.yaml sets api_key to "s3cret"
    When a caller requests GET /v1/models with "Authorization: Bearer s3cret"
    Then the response status is 200
    And the model list is returned

  @id:api-gateway.access-control.003 @rule:gateway.access.unauthenticated-refused @verified @source:internal/proxy/auth.go#withAuth @test:internal/proxy/proxy_test.go#TestInboundAuth @questionable
  Scenario Outline: A caller without a valid proxy key is refused
    Given chicco.yaml sets api_key to "s3cret"
    When a caller requests <path> presenting <credential>
    Then the response status is 401
    And the body is {"error":{"message":"chicco: missing or invalid API key","type":"chicco_error"}}
    And no provider is called

    Examples:
      | path                      | credential                                         |
      | GET /v1/models            | no Authorization header                            |
      | GET /v1/models            | "Authorization: Bearer wrong"                      |
      | POST /v1/chat/completions | "Authorization: bearer s3cret" (lower-case scheme) |
      | POST /v1/messages         | "x-api-key: s3cret" only                           |
      | GET /dashboard?key=wrong  | no cookie                                          |

  @id:api-gateway.access-control.004 @rule:gateway.access.health-always-open @verified @source:internal/proxy/auth.go#withAuth @test:internal/proxy/proxy_test.go#TestInboundAuth
  Scenario: The liveness endpoint never needs the proxy key
    Given chicco.yaml sets api_key to "s3cret"
    When a liveness probe requests GET /health without an Authorization header
    Then the response status is 200
    And the body reports an overall status and each active provider's state

  @id:api-gateway.access-control.005 @rule:gateway.access.dashboard-key-exchange @verified @source:internal/proxy/auth.go#withAuth @test:internal/proxy/proxy_test.go#TestInboundAuth
  Scenario: A browser trades the proxy key once for a dashboard session
    Given chicco.yaml sets api_key to "s3cret"
    When the operator opens /dashboard?key=s3cret in a browser
    Then the browser is redirected with status 302 to /dashboard, without the key in the address
    And the browser holds an HttpOnly, SameSite=Strict cookie named chicco_key
    And the dashboard page loads with status 200
    And the page's GET /v1/status polling is answered with status 200

  @id:api-gateway.access-control.006 @rule:gateway.access.key-change-applies-on-reload @inferred @source:internal/proxy/auth.go#currentAuthKey
  Scenario: A changed or removed proxy key takes effect on reload without a restart
    Given chicco is running with api_key "old-secret"
    And the operator changes api_key in chicco.yaml to "new-secret"
    When the operator reloads the configuration
    Then a request with "Authorization: Bearer old-secret" is refused with status 401
    And a request with "Authorization: Bearer new-secret" is served
    And a dashboard cookie holding the old key no longer authenticates
