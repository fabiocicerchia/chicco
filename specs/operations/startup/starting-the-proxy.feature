@domain:operations @area:startup @from:proxy/run
Feature: Starting the proxy
  Lets an operator start chicco safely, refusing setups that would expose provider keys or serve nothing, and making it clear from the log how the proxy is running.

  Background:
    Given chicco holds a working API key for every configured provider

  @id:operations.startup.006 @rule:operations.startup.no-open-listener-on-routable-address @verified @source:internal/proxy/run.go#RequireAuthOnBind @test:internal/proxy/bind_test.go#TestRequireAuthOnBind
  Scenario Outline: chicco refuses to listen on a reachable address without its own API key
    Given chicco.yaml sets the listen address to "<addr>"
    And chicco.yaml sets api_key to "<api_key>"
    When the operator starts chicco
    Then chicco <outcome>
    And when refused, stderr says the address is not loopback and no api_key is set, and suggests setting api_key or binding to 127.0.0.1
    And when refused, chicco exits with status 1 and no port is bound

    Examples:
      | addr               | api_key        | outcome          |
      | 127.0.0.1:41986    |                | starts serving   |
      | localhost:41986    |                | starts serving   |
      | [::1]:41986        |                | starts serving   |
      | :41986             |                | refuses to start |
      | 0.0.0.0:41986      |                | refuses to start |
      | 192.168.1.10:41986 |                | refuses to start |
      | not-an-addr        |                | refuses to start |
      | :41986             | s3cret-example | starts serving   |
      | 192.168.1.10:41986 | s3cret-example | starts serving   |

  @id:operations.startup.007 @rule:operations.startup.default-address-is-loopback @verified @source:internal/proxy/run.go#isLoopback @test:internal/proxy/bind_test.go#TestDefaultAddrIsLoopback @test:internal/proxy/bind_test.go#TestRequireAuthOnBind
  Scenario: chicco listens only on the local machine by default
    Given chicco.yaml sets no listen address and no api_key
    And the operator passes no --addr
    When the operator starts chicco
    Then chicco starts serving on a loopback address
    And it is not reachable from other machines

  @id:operations.startup.008 @rule:operations.startup.addr-flag-overrides-config @inferred @source:internal/proxy/run.go#Run
  Scenario: The listen address given on the command line wins over the config file
    Given chicco.yaml sets the listen address to "127.0.0.1:41986"
    When the operator starts chicco with --addr "127.0.0.1:50000"
    Then chicco listens on 127.0.0.1:50000
    And the startup log line names 127.0.0.1:50000

  @id:operations.startup.009 @rule:operations.startup.refuse-without-usable-providers @inferred @source:internal/proxy/run.go#Run
  Scenario: chicco refuses to start when no provider can serve requests
    Given no provider in chicco.yaml has both an API key and at least one model
    When the operator starts chicco
    Then stderr says no providers with an API key and models are configured, and points at the providers list and api_key env vars in the config file
    And chicco exits with status 1

  @id:operations.startup.010 @rule:operations.startup.log-states-auth-posture @inferred @source:internal/proxy/run.go#authState
  Scenario Outline: The startup log says whether the proxy is open or protected
    Given chicco.yaml sets api_key to "<api_key>"
    And the providers "groq" and "mistral" are usable
    When the operator starts chicco
    Then the startup log line shows the version, the listen address and "<posture>"
    And it lists the 2 providers chicco rotates across: groq, mistral

    Examples:
      | api_key        | posture           |
      |                | no api_key — open |
      | s3cret-example | api_key set       |

  @id:operations.startup.011 @rule:operations.startup.headless-when-no-terminal @inferred @source:internal/proxy/run.go#serveHeadless
  Scenario Outline: chicco runs without the dashboard when asked or when there is no terminal
    Given <situation>
    When the operator starts chicco
    Then no terminal dashboard is shown
    And log lines go to stderr
    And the web dashboard still shows recent log lines

    Examples:
      | situation                                      |
      | the operator passes --headless                 |
      | stdout is piped or run under a service manager |

  @id:operations.startup.012 @rule:operations.startup.unwritable-state-warned-once @inferred @source:internal/proxy/run.go#persistLoop
  Scenario: An unwritable usage state file is reported once
    Given chicco is started with a state file in a directory it cannot write to
    When chicco tries to save usage counters
    Then one warning names the state file and says usage counters and rate-limit windows will reset on restart
    And the warning is not repeated on later save attempts
    And chicco keeps serving requests

  @id:operations.startup.013 @rule:operations.startup.dashboard-listen-failure-not-fatal @inferred @source:internal/proxy/run.go#serveDashboard @questionable
  Scenario: With the terminal dashboard, a port that cannot be bound does not stop chicco
    Given another process already listens on 127.0.0.1:41986
    And stdout is a terminal
    When the operator starts chicco on 127.0.0.1:41986
    Then the terminal dashboard opens
    And its log pane shows a server error
    And chicco keeps running without serving requests, and does not exit with an error
