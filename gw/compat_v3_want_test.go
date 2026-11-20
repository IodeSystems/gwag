package gateway

// Observed graphql-go v2.1.1 behaviour. Keys are case names; values are
// what the upstream saw / the client got.

var (
	compatProtoNullWant = map[string]string{
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"null":   "error=\"\" | http=200 name=\"\"",
		"absent": "error=\"\" | http=200 name=\"\"",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null literal parses (Syntax Error today)
		"literal-null": "error=\"Syntax Error GraphQL request (1:25) Unexpected Name \\\"null\\\"\" | http=200 upstream not called",
		"value":        "error=\"\" | http=200 name=\"bob\"",
	}

	compatOpenAPINullWant = map[string]string{
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"query/null":   "error=\"\" | http=200 GET /q body=",
		"query/absent": "error=\"\" | http=200 GET /q body=",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null literal parses (Syntax Error today)
		"query/literal-null": "error=\"Syntax Error GraphQL request (1:14) Unexpected Name \\\"null\\\"\" | http=200 upstream not called body=",
		"query/value":        "error=\"\" | http=200 GET /q?s=a body=",
		"path/null":          "error=\"Variable \\\"$v\\\" of required type \\\"String!\\\" was not provided.\" | http=200 upstream not called body=",
		"path/absent":        "error=\"Variable \\\"$v\\\" of required type \\\"String!\\\" was not provided.\" | http=200 upstream not called body=",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"body-field/null":   "error=\"\" | http=200 POST /b body={}",
		"body-field/absent": "error=\"\" | http=200 POST /b body={}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null literal parses (Syntax Error today)
		"body-field/literal-null": "error=\"Syntax Error GraphQL request (1:32) Unexpected Name \\\"null\\\"\" | http=200 upstream not called body=",
		"body-field/value":        "error=\"\" | http=200 POST /b body={\"name\":\"a\"}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"body-nested-field/null":   "error=\"\" | http=200 POST /b body={\"nested\":{}}",
		"body-nested-field/absent": "error=\"\" | http=200 POST /b body={\"nested\":{}}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"body-nested-object/null": "error=\"\" | http=200 POST /b body={}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"body/null":   "error=\"\" | http=200 POST /b body=",
		"body/absent": "error=\"\" | http=200 POST /b body=",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"body-int/null": "error=\"\" | http=200 POST /b body={}",
	}

	compatGraphQLNullWant = map[string]string{
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"scalar/null": "error=\"\" | http=200 upstream vars={\"v\":null} query=query ($v: String) { echo(s: $v) }",
		// absent variable is forwarded upstream as null today (same as an explicit null)
		"scalar/absent": "error=\"\" | http=200 upstream vars={\"v\":null} query=query ($v: String) { echo(s: $v) }",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null literal parses (Syntax Error today)
		"scalar/literal-null": "error=\"Syntax Error GraphQL request (1:15) Unexpected Name \\\"null\\\"\" | http=200 upstream not called",
		"scalar/value":        "error=\"\" | http=200 upstream vars={\"v\":\"a\"} query=query ($v: String) { echo(s: $v) }",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"input-field/null": "error=\"\" | http=200 upstream vars={\"v\":null} query=query ($v: String) { echo(inp: {a: $v}) }",
		// absent variable is forwarded upstream as null today (same as an explicit null)
		"input-field/absent": "error=\"\" | http=200 upstream vars={\"v\":null} query=query ($v: String) { echo(inp: {a: $v}) }",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"input-nested-field/null": "error=\"\" | http=200 upstream vars={\"v\":null} query=query ($v: String) { echo(inp: {sub: {a: $v}}) }",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"input-object/null": "error=\"\" | http=200 upstream vars={\"v\":null} query=query ($v: g_In) { echo(inp: $v) }",
		// absent variable is forwarded upstream as null today (same as an explicit null)
		"input-object/absent": "error=\"\" | http=200 upstream vars={\"v\":null} query=query ($v: g_In) { echo(inp: $v) }",
	}

	compatMCPNullWant = map[string]string{
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"string/null":   "error=\"\" | http=200 args=null",
		"string/absent": "error=\"\" | http=200 args=null",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null literal parses (Syntax Error today)
		"string/literal-null": "error=\"Syntax Error GraphQL request (1:25) Unexpected Name \\\"null\\\"\" | http=200 args=upstream not called",
		"string/value":        "error=\"\" | http=200 args={\"s\":\"a\"}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: null variable reaches resolvers as present nil
		"number/null": "error=\"\" | http=200 args=null",
	}

	compatScalarGraphQLWant = map[string]string{
		"Int/2":   "error=\"\" | http=200 upstream vars={\"v\":2}",
		"Int/2.0": "error=\"\" | http=200 upstream vars={\"v\":2}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: 2.5 into Int is rejected (lossless coercion)
		"Int/2.5": "error=\"\" | http=200 upstream vars={\"v\":2}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: "2" into Int is accepted (lossless coercion)
		"Int/str2": "error=\"\" | http=200 upstream vars={\"v\":2}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: "2.5" into Int is rejected
		"Int/str2.5": "error=\"\" | http=200 upstream vars={\"v\":2}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: bool into Int/ID is rejected
		"Int/true":  "error=\"\" | http=200 upstream vars={\"v\":1}",
		"Int/1e2":   "error=\"\" | http=200 upstream vars={\"v\":100}",
		"Int/3e9":   "error=\"Variable \\\"$v\\\" got invalid value 3000000000.\" | http=200 upstream not called",
		"Float/2":   "error=\"\" | http=200 upstream vars={\"v\":2}",
		"Float/2.0": "error=\"\" | http=200 upstream vars={\"v\":2}",
		"Float/2.5": "error=\"\" | http=200 upstream vars={\"v\":2.5}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: string into Float is rejected (lossless coercion)
		"Float/str2": "error=\"\" | http=200 upstream vars={\"v\":2}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: string into Float is rejected (lossless coercion)
		"Float/str2.5": "error=\"\" | http=200 upstream vars={\"v\":2.5}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: bool into Float is rejected
		"Float/true": "error=\"\" | http=200 upstream vars={\"v\":1}",
		"Float/1e2":  "error=\"\" | http=200 upstream vars={\"v\":100}",
		"Float/3e9":  "error=\"\" | http=200 upstream vars={\"v\":3000000000}",
		"ID/2":       "error=\"\" | http=200 upstream vars={\"v\":\"2\"}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: non-string/non-int ID input is rejected
		"ID/2.0": "error=\"\" | http=200 upstream vars={\"v\":\"2\"}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: non-string/non-int ID input is rejected
		"ID/2.5":    "error=\"\" | http=200 upstream vars={\"v\":\"2.5\"}",
		"ID/str2":   "error=\"\" | http=200 upstream vars={\"v\":\"2\"}",
		"ID/str2.5": "error=\"\" | http=200 upstream vars={\"v\":\"2.5\"}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: non-string/non-int ID input is rejected
		"ID/true": "error=\"\" | http=200 upstream vars={\"v\":\"true\"}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: non-string/non-int ID input is rejected
		"ID/1e2": "error=\"\" | http=200 upstream vars={\"v\":\"100\"}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: non-string/non-int ID input is rejected
		"ID/3e9": "error=\"\" | http=200 upstream vars={\"v\":\"3e+09\"}",
	}

	compatScalarOpenAPIWant = map[string]string{
		"Int/2":   "error=\"\" | http=200 GET /q?i=2",
		"Int/2.0": "error=\"\" | http=200 GET /q?i=2",
		// v2.1.1 behaviour; expected to change in graphql-go v3: 2.5 into Int is rejected (lossless coercion)
		"Int/2.5": "error=\"\" | http=200 GET /q?i=2",
		// v2.1.1 behaviour; expected to change in graphql-go v3: "2" into Int is accepted (lossless coercion)
		"Int/str2": "error=\"\" | http=200 GET /q?i=2",
		// v2.1.1 behaviour; expected to change in graphql-go v3: "2.5" into Int is rejected
		"Int/str2.5": "error=\"\" | http=200 GET /q?i=2",
		// v2.1.1 behaviour; expected to change in graphql-go v3: bool into Int/ID is rejected
		"Int/true":  "error=\"\" | http=200 GET /q?i=1",
		"Int/1e2":   "error=\"\" | http=200 GET /q?i=100",
		"Int/3e9":   "error=\"Variable \\\"$v\\\" got invalid value 3000000000.\" | http=200 upstream not called",
		"Float/2":   "error=\"\" | http=200 GET /q?f=2",
		"Float/2.0": "error=\"\" | http=200 GET /q?f=2",
		"Float/2.5": "error=\"\" | http=200 GET /q?f=2.5",
		// v2.1.1 behaviour; expected to change in graphql-go v3: string into Float is rejected (lossless coercion)
		"Float/str2": "error=\"\" | http=200 GET /q?f=2",
		// v2.1.1 behaviour; expected to change in graphql-go v3: string into Float is rejected (lossless coercion)
		"Float/str2.5": "error=\"\" | http=200 GET /q?f=2.5",
		// v2.1.1 behaviour; expected to change in graphql-go v3: bool into Float is rejected
		"Float/true": "error=\"\" | http=200 GET /q?f=1",
		"Float/1e2":  "error=\"\" | http=200 GET /q?f=100",
		// Fixed (was 3e%2B09): numbers reach upstreams without exponent form.
		"Float/3e9": "error=\"\" | http=200 GET /q?f=3000000000",
	}

	compatScalarMCPWant = map[string]string{
		"Float/2":   "error=\"\" | http=200 args={\"n\":2}",
		"Float/2.0": "error=\"\" | http=200 args={\"n\":2}",
		"Float/2.5": "error=\"\" | http=200 args={\"n\":2.5}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: string into Float is rejected (lossless coercion)
		"Float/str2": "error=\"\" | http=200 args={\"n\":2}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: string into Float is rejected (lossless coercion)
		"Float/str2.5": "error=\"\" | http=200 args={\"n\":2.5}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: bool into Float is rejected
		"Float/true": "error=\"\" | http=200 args={\"n\":1}",
		"Float/1e2":  "error=\"\" | http=200 args={\"n\":100}",
		"Float/3e9":  "error=\"\" | http=200 args={\"n\":3000000000}",
	}

	compatNullObjGraphQLWant = map[string]string{
		"outer-null":    "http=200 {\"data\":{\"g\":{\"outer\":null}}}",
		"outer-empty":   "http=200 {\"data\":{\"g\":{\"outer\":{}}}}",
		"inner-null":    "http=200 {\"data\":{\"g\":{\"outer\":{\"inner\":null,\"innerNN\":{\"x\":\"a\"}}}}}",
		"inner-empty":   "http=200 {\"data\":{\"g\":{\"outer\":{\"inner\":{},\"innerNN\":{\"x\":\"a\"}}}}}",
		"innerNN-null":  "http=200 {\"data\":{\"g\":{\"outer\":{\"inner\":null,\"innerNN\":null}}}}",
		"innerNN-empty": "http=200 {\"data\":{\"g\":{\"outer\":{\"inner\":null,\"innerNN\":{}}}}}",
		"outerNN-null":  "http=200 {\"data\":{\"g\":{\"outerNN\":null}}}",
		"outerNN-empty": "http=200 {\"data\":{\"g\":{\"outerNN\":{}}}}",
		"data-null":     "http=200 {\"data\":{\"g\":{\"outer\":null}}}",
	}

	compatNullObjOpenAPIWant = map[string]string{
		// v2.1.1 behaviour; expected to change in graphql-go v3: nil map -> null
		"outer-null": "http=200 {\"data\":{\"t\":{\"obj\":null}}}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: nil map -> null
		"outer-empty": "http=200 {\"data\":{\"t\":{\"obj\":{\"name\":null,\"inner\":null,\"innerNN\":null}}}}",
		"inner-null":  "http=200 {\"data\":{\"t\":{\"obj\":{\"name\":null,\"inner\":null,\"innerNN\":{\"x\":\"a\"}}}}}",
		// v2.1.1 behaviour; expected to change in graphql-go v3: nil map -> null
		"inner-empty":     "http=200 {\"data\":{\"t\":{\"obj\":{\"name\":null,\"inner\":{\"x\":null},\"innerNN\":{\"x\":\"a\"}}}}}",
		"innerNN-null":    "http=200 {\"data\":{\"t\":{\"obj\":{\"name\":null,\"inner\":null,\"innerNN\":null}}}}",
		"innerNN-empty":   "http=200 {\"data\":{\"t\":{\"obj\":{\"name\":null,\"inner\":null,\"innerNN\":{\"x\":null}}}}}",
		"innerNN-missing": "http=200 {\"data\":{\"t\":{\"obj\":{\"name\":null,\"inner\":null,\"innerNN\":null}}}}",
	}
)
