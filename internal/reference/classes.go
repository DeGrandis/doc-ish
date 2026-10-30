package reference

import "strings"

// Drug labels often warn about a class ("NSAIDs", "ACE inhibitors")
// rather than naming each drug, so an interaction check also looks for the
// classes a drug belongs to. These are the common classes and the phrases
// labels use for them; members are generic names, lower case.
var drugClasses = []struct {
	phrases []string
	members []string
}{
	{[]string{"NSAID", "NSAIDs", "nonsteroidal anti-inflammatory", "non-steroidal anti-inflammatory"},
		[]string{"ibuprofen", "naproxen", "aspirin", "diclofenac", "celecoxib", "meloxicam", "indomethacin", "ketorolac", "etodolac", "nabumetone", "piroxicam"}},
	{[]string{"ACE inhibitor", "ACE inhibitors", "angiotensin-converting enzyme inhibitor", "angiotensin converting enzyme inhibitor"},
		[]string{"lisinopril", "enalapril", "ramipril", "benazepril", "captopril", "quinapril", "fosinopril", "perindopril", "trandolapril"}},
	{[]string{"angiotensin receptor blocker", "angiotensin II receptor blocker", "ARB", "ARBs"},
		[]string{"losartan", "valsartan", "irbesartan", "olmesartan", "telmisartan", "candesartan", "azilsartan"}},
	{[]string{"statin", "statins", "HMG-CoA reductase inhibitor"},
		[]string{"atorvastatin", "simvastatin", "rosuvastatin", "pravastatin", "lovastatin", "fluvastatin", "pitavastatin"}},
	{[]string{"diuretic", "diuretics", "thiazide"},
		[]string{"hydrochlorothiazide", "chlorthalidone", "furosemide", "bumetanide", "torsemide", "spironolactone", "indapamide"}},
	{[]string{"potassium-sparing diuretic", "potassium sparing diuretic"},
		[]string{"spironolactone", "eplerenone", "amiloride", "triamterene"}},
	{[]string{"beta blocker", "beta-blocker", "beta-adrenergic blocking"},
		[]string{"metoprolol", "atenolol", "propranolol", "carvedilol", "bisoprolol", "nebivolol", "labetalol"}},
	{[]string{"anticoagulant", "anticoagulants", "blood thinner"},
		[]string{"warfarin", "apixaban", "rivaroxaban", "dabigatran", "edoxaban", "heparin"}},
	{[]string{"SSRI", "SSRIs", "selective serotonin reuptake inhibitor"},
		[]string{"sertraline", "fluoxetine", "citalopram", "escitalopram", "paroxetine", "fluvoxamine"}},
	{[]string{"SNRI", "SNRIs", "serotonin and norepinephrine reuptake inhibitor"},
		[]string{"venlafaxine", "duloxetine", "desvenlafaxine", "levomilnacipran"}},
	{[]string{"opioid", "opioids"},
		[]string{"oxycodone", "hydrocodone", "morphine", "tramadol", "codeine", "fentanyl", "methadone", "hydromorphone"}},
	{[]string{"benzodiazepine", "benzodiazepines"},
		[]string{"alprazolam", "lorazepam", "diazepam", "clonazepam", "temazepam"}},
	{[]string{"strong CYP3A4 inhibitor", "CYP3A4 inhibitor", "CYP3A4 inhibitors"},
		[]string{"clarithromycin", "erythromycin", "itraconazole", "ketoconazole", "ritonavir", "grapefruit"}},
	{[]string{"proton pump inhibitor", "proton pump inhibitors", "PPI", "PPIs"},
		[]string{"omeprazole", "esomeprazole", "lansoprazole", "pantoprazole", "rabeprazole"}},
	{[]string{"sulfonylurea", "sulfonylureas"},
		[]string{"glipizide", "glyburide", "glimepiride"}},
	{[]string{"insulin"},
		[]string{"insulin glargine", "insulin lispro", "insulin aspart", "insulin detemir", "insulin degludec"}},
}

// classPhrases are the ways labels name the classes a drug belongs to.
func classPhrases(names []string) []string {
	var out []string
	for _, c := range drugClasses {
		for _, m := range c.members {
			if matchesAny(m, names) {
				out = append(out, c.phrases...)
				break
			}
		}
	}
	return out
}

// matchesAny reports whether a generic name is among names, allowing for
// a salt ("atorvastatin" matches "atorvastatin calcium").
func matchesAny(generic string, names []string) bool {
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == generic || strings.HasPrefix(n, generic+" ") {
			return true
		}
	}
	return false
}
