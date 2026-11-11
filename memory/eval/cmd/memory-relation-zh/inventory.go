package main

// zhInventory is the vocabulary reading's fixed half: the predicate phrases a
// Chinese extractor plausibly writes for each single-valued relation the
// maintenance pass knows, plus phrases that look like relations and are not
// (they hold several values at once, or they state a value rather than state
// which relation it is). It is a hand-authored inventory rather than a
// sampled one, so it is a lower bound on what a model writes and an upper
// bound on nothing: the live half of the reading measures the real thing.
//
// An entry is in the inventory because a Chinese speaker would read the phrase
// as stating that relation and no other, not because the extractor is known to
// emit it. The judgment is the same one DefaultRelations documents: a
// single-valued relation about one subject, phrased as a verb without its
// object.
type inventoryEntry struct {
	// Relation is the class the phrase states, or "" for a negative entry.
	Relation string
	// Predicate is the phrase as the predicate field would carry it.
	Predicate string
	// Note says why the entry is here.
	Note string
	// Control marks the English entries: they are in the corpus so the reading
	// can show the vocabulary is not simply broken, and they are counted apart
	// from the Chinese inventory, which is the measurement.
	Control bool
}

// zhRelationInventory is the inventory the reading is computed over.
var zhRelationInventory = []inventoryEntry{
	// residence: where the subject lives now.
	{Relation: "residence", Predicate: "住在", Note: "the plain statement of where someone lives"},
	{Relation: "residence", Predicate: "居住在", Note: "the written register of 住在"},
	{Relation: "residence", Predicate: "居住于", Note: "居住在 with the object marked by 于"},
	{Relation: "residence", Predicate: "搬到", Note: "the move that established the current residence"},
	{Relation: "residence", Predicate: "搬到了", Note: "搬到 with the completed aspect"},
	{Relation: "residence", Predicate: "搬去", Note: "southern for 搬到; the same current residence"},
	{Relation: "residence", Predicate: "搬来", Note: "the same move from the destination's point of view"},
	{Relation: "residence", Predicate: "定居在", Note: "settled in, which states the residence the settling made permanent"},
	{Relation: "residence", Predicate: "迁居", Note: "the written verb for relocating"},
	{Relation: "residence", Predicate: "移居", Note: "relocated, usually across regions"},
	{Relation: "residence", Predicate: "现居", Note: "the profile-line form: currently resident in"},
	{Relation: "residence", Predicate: "安家", Note: "made a home in"},
	// employer: who employs the subject.
	{Relation: "employer", Predicate: "就职于", Note: "the written form of works at"},
	{Relation: "employer", Predicate: "任职于", Note: "holds a post at"},
	{Relation: "employer", Predicate: "供职于", Note: "serves at"},
	{Relation: "employer", Predicate: "工作于", Note: "works at, object marked by 于"},
	{Relation: "employer", Predicate: "入职", Note: "joined as an employee, which states the employer"},
	// role: what the subject works as.
	{Relation: "role", Predicate: "担任", Note: "holds the position of"},
	{Relation: "role", Predicate: "职位是", Note: "the position is"},
	{Relation: "role", Predicate: "职务是", Note: "the post is"},
	{Relation: "role", Predicate: "是一名", Note: "works as a ..., the unmarked Chinese copula"},
	// marital status: one partner at a time.
	{Relation: "marital status", Predicate: "结婚", Note: "married, with no object in the predicate"},
	{Relation: "marital status", Predicate: "已婚", Note: "the profile-line form"},
	{Relation: "marital status", Predicate: "订婚", Note: "engaged"},
	{Relation: "marital status", Predicate: "嫁给", Note: "married (of a woman), before the object"},
	{Relation: "marital status", Predicate: "娶了", Note: "married (of a man), before the object"},
	// birth: one place, one date -- restated, never updated.
	{Relation: "birth", Predicate: "出生在", Note: "born in a place"},
	{Relation: "birth", Predicate: "出生于", Note: "born in a place, object marked by 于"},
	{Relation: "birth", Predicate: "生于", Note: "the written form of born in"},
	{Relation: "birth", Predicate: "生日是", Note: "the birthday is"},
	{Relation: "birth", Predicate: "出生日期是", Note: "the date of birth is"},

	// Negative entries: phrases a model writes that must not be read as a
	// single-valued relation. A vocabulary that fires here would replace a
	// current value with one of several.
	{Relation: "", Predicate: "加入", Note: "joined a club, a team, a group -- several at once"},
	{Relation: "", Predicate: "参加", Note: "attended or took part in -- repeatable"},
	{Relation: "", Predicate: "喜欢", Note: "a preference: several are current together"},
	{Relation: "", Predicate: "养了", Note: "keeps an animal -- several at once"},
	{Relation: "", Predicate: "有", Note: "the generic possession verb: children, siblings, opinions"},
	{Relation: "", Predicate: "去过", Note: "has been to -- an event, repeatable"},
	{Relation: "", Predicate: "打算", Note: "plans to -- a plan, and several plans coexist"},
	{Relation: "", Predicate: "毕业于", Note: "graduated from -- a fact about the past, not a current value"},

	// English stays in the inventory: the reading is a comparison, and a
	// vocabulary that recognizes Chinese by forgetting English is not a
	// vocabulary.
	{Relation: "residence", Predicate: "lives in", Note: "the English default, for the comparison", Control: true},
	{Relation: "employer", Predicate: "works at", Note: "the English default, for the comparison", Control: true},
	{Relation: "", Predicate: "attended", Note: "the English negative control", Control: true},
}
