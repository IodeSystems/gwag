package gateway

// Observed behaviour on graphql-go v3.1.0. Keys are case names; values
// are what the upstream saw / the client got. A "v3:" comment marks a
// case that changed from v2.1.1 and says what it was.

var (
	compatProtoNullWant = map[string]string{
		// v3: null reaches the resolver as a present nil; proto treats
		// it as unset (field omitted), the same result as v2.1.1.
		"null":   `error="" | http=200 name=""`,
		"absent": `error="" | http=200 name=""`,
		// v3: null literal parses and means unset (v2.1.1: Syntax Error).
		"literal-null": `error="" | http=200 name=""`,
		"value":        `error="" | http=200 name="bob"`,
	}

	compatOpenAPINullWant = map[string]string{
		// v3: null reaches the resolver as a present nil; a null query
		// param is not sent, the same result as v2.1.1.
		"query/null":   `error="" | http=200 GET /q body=`,
		"query/absent": `error="" | http=200 GET /q body=`,
		// v3: null literal parses and sends no param (v2.1.1: Syntax Error).
		"query/literal-null": `error="" | http=200 GET /q body=`,
		"query/value":        `error="" | http=200 GET /q?s=a body=`,
		"path/null":          `error="Variable \"$v\" of required type \"String!\" was not provided." | http=200 upstream not called body=`,
		"path/absent":        `error="Variable \"$v\" of required type \"String!\" was not provided." | http=200 upstream not called body=`,
		// v3: a null body field is forwarded as null (v2.1.1: dropped, body={}).
		"body-field/null":   `error="" | http=200 POST /b body={"name":null}`,
		"body-field/absent": `error="" | http=200 POST /b body={}`,
		// v3: null literal parses and is forwarded (v2.1.1: Syntax Error).
		"body-field/literal-null": `error="" | http=200 POST /b body={"name":null}`,
		"body-field/value":        `error="" | http=200 POST /b body={"name":"a"}`,
		// v3: forwarded as null (v2.1.1: body={"nested":{}}).
		"body-nested-field/null":   `error="" | http=200 POST /b body={"nested":{"x":null}}`,
		"body-nested-field/absent": `error="" | http=200 POST /b body={"nested":{}}`,
		// v3: forwarded as null (v2.1.1: body={}).
		"body-nested-object/null": `error="" | http=200 POST /b body={"nested":null}`,
		// A null body argument sends no body, as on v2.1.1.
		"body/null":   `error="" | http=200 POST /b body=`,
		"body/absent": `error="" | http=200 POST /b body=`,
		// v3: forwarded as null (v2.1.1: body={}).
		"body-int/null": `error="" | http=200 POST /b body={"count":null}`,
	}

	compatGraphQLNullWant = map[string]string{
		"scalar/null": `error="" | http=200 upstream vars={"v":null} query=query ($v: String) { echo(s: $v) }`,
		// v3: an absent variable is not forwarded (v2.1.1: vars={"v":null}).
		"scalar/absent": `error="" | http=200 upstream vars= query=query ($v: String) { echo(s: $v) }`,
		// v3: null literal parses and is forwarded in the query text
		// (v2.1.1: Syntax Error).
		"scalar/literal-null": `error="" | http=200 upstream vars= query={ echo(s: null) }`,
		"scalar/value":        `error="" | http=200 upstream vars={"v":"a"} query=query ($v: String) { echo(s: $v) }`,
		"input-field/null":    `error="" | http=200 upstream vars={"v":null} query=query ($v: String) { echo(inp: {a: $v}) }`,
		// v3: an absent variable is not forwarded (v2.1.1: vars={"v":null}).
		"input-field/absent":      `error="" | http=200 upstream vars= query=query ($v: String) { echo(inp: {a: $v}) }`,
		"input-nested-field/null": `error="" | http=200 upstream vars={"v":null} query=query ($v: String) { echo(inp: {sub: {a: $v}}) }`,
		"input-object/null":       `error="" | http=200 upstream vars={"v":null} query=query ($v: g_In) { echo(inp: $v) }`,
		// v3: an absent variable is not forwarded (v2.1.1: vars={"v":null}).
		"input-object/absent": `error="" | http=200 upstream vars= query=query ($v: g_In) { echo(inp: $v) }`,
	}

	compatMCPNullWant = map[string]string{
		// v3: a null tool argument is forwarded as null (v2.1.1: args=null,
		// no arguments sent).
		"string/null":   `error="" | http=200 args={"s":null}`,
		"string/absent": `error="" | http=200 args=null`,
		// v3: null literal parses and is forwarded (v2.1.1: Syntax Error).
		"string/literal-null": `error="" | http=200 args={"s":null}`,
		"string/value":        `error="" | http=200 args={"s":"a"}`,
		// v3: forwarded as null (v2.1.1: args=null).
		"number/null": `error="" | http=200 args={"n":null}`,
	}

	compatScalarGraphQLWant = map[string]string{
		"Int/2":   `error="" | http=200 upstream vars={"v":2}`,
		"Int/2.0": `error="" | http=200 upstream vars={"v":2}`,
		// v3: lossy, rejected (v2.1.1: truncated to 2).
		"Int/2.5":  `error="Variable \"$v\" got invalid value 2.5." | http=200 upstream not called`,
		"Int/str2": `error="" | http=200 upstream vars={"v":2}`,
		// v3: lossy, rejected (v2.1.1: truncated to 2).
		"Int/str2.5": `error="Variable \"$v\" got invalid value \"2.5\"." | http=200 upstream not called`,
		// v3: rejected (v2.1.1: 1).
		"Int/true":     `error="Variable \"$v\" got invalid value true." | http=200 upstream not called`,
		"Int/1e2":      `error="" | http=200 upstream vars={"v":100}`,
		"Int/3e9":      `error="Variable \"$v\" got invalid value 3000000000." | http=200 upstream not called`,
		"Float/2":      `error="" | http=200 upstream vars={"v":2}`,
		"Float/2.0":    `error="" | http=200 upstream vars={"v":2}`,
		"Float/2.5":    `error="" | http=200 upstream vars={"v":2.5}`,
		"Float/str2":   `error="" | http=200 upstream vars={"v":2}`,
		"Float/str2.5": `error="" | http=200 upstream vars={"v":2.5}`,
		// v3: rejected (v2.1.1: 1).
		"Float/true": `error="Variable \"$v\" got invalid value true." | http=200 upstream not called`,
		"Float/1e2":  `error="" | http=200 upstream vars={"v":100}`,
		"Float/3e9":  `error="" | http=200 upstream vars={"v":3000000000}`,
		"ID/2":       `error="" | http=200 upstream vars={"v":"2"}`,
		"ID/2.0":     `error="" | http=200 upstream vars={"v":"2"}`,
		// v3: fractional, rejected (v2.1.1: "2.5").
		"ID/2.5":    `error="Variable \"$v\" got invalid value 2.5." | http=200 upstream not called`,
		"ID/str2":   `error="" | http=200 upstream vars={"v":"2"}`,
		"ID/str2.5": `error="" | http=200 upstream vars={"v":"2.5"}`,
		// v3: rejected (v2.1.1: "true").
		"ID/true": `error="Variable \"$v\" got invalid value true." | http=200 upstream not called`,
		"ID/1e2":  `error="" | http=200 upstream vars={"v":"100"}`,
		// v3: decimal digits (v2.1.1: "3e+09").
		"ID/3e9": `error="" | http=200 upstream vars={"v":"3000000000"}`,
	}

	compatScalarOpenAPIWant = map[string]string{
		"Int/2":   `error="" | http=200 GET /q?i=2`,
		"Int/2.0": `error="" | http=200 GET /q?i=2`,
		// v3: lossy, rejected (v2.1.1: truncated to 2).
		"Int/2.5":  `error="Variable \"$v\" got invalid value 2.5." | http=200 upstream not called`,
		"Int/str2": `error="" | http=200 GET /q?i=2`,
		// v3: lossy, rejected (v2.1.1: truncated to 2).
		"Int/str2.5": `error="Variable \"$v\" got invalid value \"2.5\"." | http=200 upstream not called`,
		// v3: rejected (v2.1.1: 1).
		"Int/true":     `error="Variable \"$v\" got invalid value true." | http=200 upstream not called`,
		"Int/1e2":      `error="" | http=200 GET /q?i=100`,
		"Int/3e9":      `error="Variable \"$v\" got invalid value 3000000000." | http=200 upstream not called`,
		"Float/2":      `error="" | http=200 GET /q?f=2`,
		"Float/2.0":    `error="" | http=200 GET /q?f=2`,
		"Float/2.5":    `error="" | http=200 GET /q?f=2.5`,
		"Float/str2":   `error="" | http=200 GET /q?f=2`,
		"Float/str2.5": `error="" | http=200 GET /q?f=2.5`,
		// v3: rejected (v2.1.1: 1).
		"Float/true": `error="Variable \"$v\" got invalid value true." | http=200 upstream not called`,
		"Float/1e2":  `error="" | http=200 GET /q?f=100`,
		// Was 3e%2B09 before 5433866b (numbers without exponent form).
		"Float/3e9": `error="" | http=200 GET /q?f=3000000000`,
	}

	compatScalarMCPWant = map[string]string{
		"Float/2":      `error="" | http=200 args={"n":2}`,
		"Float/2.0":    `error="" | http=200 args={"n":2}`,
		"Float/2.5":    `error="" | http=200 args={"n":2.5}`,
		"Float/str2":   `error="" | http=200 args={"n":2}`,
		"Float/str2.5": `error="" | http=200 args={"n":2.5}`,
		// v3: rejected (v2.1.1: 1).
		"Float/true": `error="Variable \"$v\" got invalid value true." | http=200 args=upstream not called`,
		"Float/1e2":  `error="" | http=200 args={"n":100}`,
		"Float/3e9":  `error="" | http=200 args={"n":3000000000}`,
	}

	// Results: the GraphQL and OpenAPI upstream paths assemble the
	// response without graphql-go's nil-map handling, so v3's
	// "nil map -> null" does not show here; unchanged from v2.1.1.
	compatNullObjGraphQLWant = map[string]string{
		"outer-null":    `http=200 {"data":{"g":{"outer":null}}}`,
		"outer-empty":   `http=200 {"data":{"g":{"outer":{}}}}`,
		"inner-null":    `http=200 {"data":{"g":{"outer":{"inner":null,"innerNN":{"x":"a"}}}}}`,
		"inner-empty":   `http=200 {"data":{"g":{"outer":{"inner":{},"innerNN":{"x":"a"}}}}}`,
		"innerNN-null":  `http=200 {"data":{"g":{"outer":{"inner":null,"innerNN":null}}}}`,
		"innerNN-empty": `http=200 {"data":{"g":{"outer":{"inner":null,"innerNN":{}}}}}`,
		"outerNN-null":  `http=200 {"data":{"g":{"outerNN":null}}}`,
		"outerNN-empty": `http=200 {"data":{"g":{"outerNN":{}}}}`,
		"data-null":     `http=200 {"data":{"g":{"outer":null}}}`,
	}

	compatNullObjOpenAPIWant = map[string]string{
		"outer-null":      `http=200 {"data":{"t":{"obj":null}}}`,
		"outer-empty":     `http=200 {"data":{"t":{"obj":{"name":null,"inner":null,"innerNN":null}}}}`,
		"inner-null":      `http=200 {"data":{"t":{"obj":{"name":null,"inner":null,"innerNN":{"x":"a"}}}}}`,
		"inner-empty":     `http=200 {"data":{"t":{"obj":{"name":null,"inner":{"x":null},"innerNN":{"x":"a"}}}}}`,
		"innerNN-null":    `http=200 {"data":{"t":{"obj":{"name":null,"inner":null,"innerNN":null}}}}`,
		"innerNN-empty":   `http=200 {"data":{"t":{"obj":{"name":null,"inner":null,"innerNN":{"x":null}}}}}`,
		"innerNN-missing": `http=200 {"data":{"t":{"obj":{"name":null,"inner":null,"innerNN":null}}}}`,
	}
)
