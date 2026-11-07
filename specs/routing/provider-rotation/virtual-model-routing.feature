@domain:routing @area:provider-rotation @from:proxy/route
Feature: Routing requests through virtual models
  Lets operators decide which providers and models serve each virtual model, and in what order of preference, behind one stable model name.

  Background:
    Given chicco is running with the providers "groq", "cerebras" and "openrouter" active, in that config order

  @id:routing.provider-rotation.002 @rule:routing.virtual-models.only-listed-backends @inferred @source:internal/proxy/route.go#activeForModel
  Scenario: A virtual model is served only by its listed backend providers and models
    Given the virtual model "coder" lists the backends groq/llama-3.3-70b and openrouter/qwen-2.5-coder
    And "groq" also offers the model "gemma2-9b"
    When a client requests a completion with the model "coder"
    Then the request goes to "groq" with the model "llama-3.3-70b" or to "openrouter" with the model "qwen-2.5-coder"
    And "cerebras" is never tried
    And "gemma2-9b" is never requested

  @id:routing.provider-rotation.003 @rule:routing.virtual-models.unmatched-model-uses-whole-pool @verified @source:internal/proxy/route.go#activeForModel @test:internal/proxy/alias_test.go#TestUnknownModelStillFallsBackRatherThanFailing @test:internal/proxy/strategy_test.go#TestStrategyPerModel @questionable
  Scenario Outline: The auto model, an empty model or an unrecognised model name is served by the whole provider pool in config order
    Given no virtual model or alias is named <model>
    When a client requests a completion with the model <model>
    Then "groq" is tried first, then "cerebras", then "openrouter"
    And each provider is asked for one of its own configured models
    And the request is not refused for naming an unknown model

    Examples:
      | model                         |
      | "chicco:auto"                 |
      | ""                            |
      | "something-nobody-configured" |

  @id:routing.provider-rotation.004 @rule:routing.virtual-models.strategy-sets-preference @verified @source:internal/proxy/route.go#order @test:internal/proxy/strategy_test.go#TestStrategyDefaultIsConfigOrder @test:internal/proxy/strategy_test.go#TestStrategyRoundRobin @test:internal/proxy/strategy_test.go#TestStrategyRandomIsPermutation @test:internal/proxy/strategy_test.go#TestStrategyWeighted @test:internal/proxy/strategy_test.go#TestStrategyPerModel
  Scenario Outline: A virtual model's load-balancing strategy decides which provider is tried first
    Given the virtual model "coder" lists the backends "groq", "cerebras" and "openrouter" in that order, with strategy <strategy>
    And no provider is cooling down
    When a client sends successive completion requests with the model "coder"
    Then the first provider tried is <first_provider>
    And providers that fail are still followed by the others in that request

    Examples:
      | strategy         | first_provider                                                                                  |
      | order (or unset) | always "groq", until it is rate-limited into cooldown                                           |
      | round_robin      | "groq", then "cerebras", then "openrouter", then "groq" again                                   |
      | random           | any of the three, in a fresh random order each request                                          |
      | weighted         | drawn in proportion to provider weight, so weight 9 leads about nine times as often as weight 1 |

  @id:routing.provider-rotation.005 @rule:routing.virtual-models.backend-weight-override @verified @source:internal/proxy/route.go#activeForModel @test:internal/proxy/strategy_test.go#TestBackendWeightOverride
  Scenario: A backend weight set on a virtual model overrides the provider's weight for that model only
    Given "groq" and "openrouter" both have provider weight 1
    And the weighted virtual model "favor-groq" gives the backend "groq" weight 9 and "openrouter" weight 1
    And the weighted virtual model "even" sets no backend weights
    When a client sends many completion requests to each virtual model
    Then "groq" is tried first for about 90% of "favor-groq" requests
    And "groq" is tried first for about 50% of "even" requests

  @id:routing.provider-rotation.006 @rule:routing.virtual-models.rotate-models-within-provider @inferred @source:internal/proxy/route.go#pick
  Scenario: A provider with several backend models takes them in turn
    Given the virtual model "coder" lists the backends groq/llama-3.3-70b and groq/gemma2-9b only
    When a client sends successive completion requests with the model "coder"
    Then the requests to "groq" alternate between "gemma2-9b" and "llama-3.3-70b"
