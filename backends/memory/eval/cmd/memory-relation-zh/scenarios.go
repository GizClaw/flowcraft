package main

// zhScenario is one Chinese conversation the extraction half of the reading
// runs the production extractor over. Every scenario states a single-valued
// relation about one named subject, and the later turn changes the value: the
// extractor is asked for the facts, and the reading asks whether the
// maintenance vocabulary can act on what it wrote.
//
// The turns are short on purpose. A long conversation gives the extractor more
// to choose from, and the reading is about the predicate vocabulary rather than
// about how much a model extracts; one turn stating one relation keeps the
// failure attributable to the vocabulary instead of to the sample.
type zhScenario struct {
	ID    string
	Turns []zhTurn
}

// zhTurn is one message with the anchor the production derivation source
// renders in front of it ("[<observed_at>] text"), so relative references
// resolve the way they do on a live turn.
type zhTurn struct {
	// At is the observation anchor, date-only: these are conversations, and a
	// date is what a Chinese speaker's "去年" resolves against.
	At string
	// Says is the message body.
	Says string
	// Want is the single-valued relation this turn states, as the vocabulary
	// would have to classify it, or "" when the turn states none. It is the
	// label the reading scores against, and it is set by construction -- the
	// turn was written to state that relation -- not by asking the vocabulary
	// under test what it thinks.
	Want string
}

// zhScenarios is the live half's corpus: nine relation statements that must be
// detected as updates, plus three conversations whose repeated verbs must not
// be read as one.
var zhScenarios = []zhScenario{
	{
		ID: "zh-residence-moved",
		Turns: []zhTurn{
			{At: "2021-05-01", Says: "我叫王磊，现在住在杭州，做外贸生意。", Want: "residence"},
			{At: "2023-08-14", Says: "上个月我从杭州搬到了成都，公司在成都开了分部。", Want: "residence"},
		},
	},
	{
		ID: "zh-residence-settled",
		Turns: []zhTurn{
			{At: "2020-02-01", Says: "李娜一直住在广州，在一家设计公司上班。", Want: "residence"},
			{At: "2022-11-03", Says: "李娜去年定居在上海了，房子也买在那边。", Want: "residence"},
		},
	},
	{
		ID: "zh-residence-written",
		Turns: []zhTurn{
			{At: "2019-11-01", Says: "赵启明现居深圳，任职于一家芯片公司。", Want: "residence"},
			{At: "2022-04-16", Says: "赵启明已经移居新加坡，太太和孩子都在那边。", Want: "residence"},
		},
	},
	{
		ID: "zh-employer-jumped",
		Turns: []zhTurn{
			{At: "2021-01-10", Says: "张伟在腾讯做后端开发，已经三年了。", Want: "employer"},
			{At: "2023-04-02", Says: "张伟今年跳槽去了字节跳动，还是做后端。", Want: "employer"},
		},
	},
	{
		ID: "zh-employer-joined",
		Turns: []zhTurn{
			{At: "2020-06-01", Says: "陈曦在一家叫云图的创业公司上班，做数据分析。", Want: "employer"},
			{At: "2022-09-20", Says: "陈曦上个月入职了阿里巴巴，岗位没变。", Want: "employer"},
		},
	},
	{
		ID: "zh-employer-served",
		Turns: []zhTurn{
			{At: "2017-08-01", Says: "何静供职于一家国企，做财务。", Want: "employer"},
			{At: "2021-12-01", Says: "何静换到了外企，就职于一家德国公司的上海分部。", Want: "employer"},
		},
	},
	{
		ID: "zh-role-switched",
		Turns: []zhTurn{
			{At: "2019-03-05", Says: "周敏在银行上班，是一名会计。", Want: "role"},
			{At: "2023-02-18", Says: "周敏去年转岗做了产品经理，现在带一个小团队。", Want: "role"},
		},
	},
	{
		ID: "zh-role-title",
		Turns: []zhTurn{
			{At: "2021-07-01", Says: "吴强的职位是销售专员，负责华东区。", Want: "role"},
			{At: "2024-01-09", Says: "吴强今年升上去了，担任销售总监。", Want: "role"},
		},
	},
	{
		ID: "zh-marital-engaged-then-married",
		Turns: []zhTurn{
			{At: "2018-10-01", Says: "郑凯和女友林悦订婚了，准备明年办婚礼。", Want: "marital status"},
			{At: "2021-05-20", Says: "郑凯和林悦已经结婚了，婚礼在厦门办的。", Want: "marital status"},
		},
	},
	{
		ID: "zh-birth-corrected",
		Turns: []zhTurn{
			{At: "2020-03-01", Says: "孙悦出生在南京，小时候在那边长大。", Want: "birth"},
			{At: "2023-06-11", Says: "孙悦其实是在苏州出生的，之前跟你说错了。", Want: "birth"},
		},
	},
	// Negative controls: repeated verbs that are not one relation with one
	// value. A vocabulary that fires here replaces a current value with one of
	// several, which is the failure the class rule exists to avoid.
	{
		ID: "zh-negative-pets",
		Turns: []zhTurn{
			{At: "2020-01-05", Says: "我很喜欢猫，家里养了两只。"},
			{At: "2022-03-08", Says: "我上个月又养了一只狗，现在家里更热闹了。"},
		},
	},
	{
		ID: "zh-negative-events",
		Turns: []zhTurn{
			{At: "2021-02-02", Says: "我加入了社区合唱团，每周三排练。"},
			{At: "2023-09-09", Says: "我又参加了公司的跑团，周末一起去跑步。"},
		},
	},
	{
		ID: "zh-negative-plans",
		Turns: []zhTurn{
			{At: "2022-05-05", Says: "我打算明年去日本读书，正在准备语言考试。"},
			{At: "2023-10-10", Says: "我打算后年去加拿大工作，那边有个机会。"},
		},
	},
}
