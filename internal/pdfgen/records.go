package pdfgen

import (
	"fmt"
	"strings"
)

// An invented 24-page health records release for Jordan Example, born
// 1979-06-02, made to be hard the way real ones are:
//
//   - every page has the clinic's header and a footer with the page
//     number and print date;
//   - a cumulative lab table (three collection dates as columns) runs
//     over pages 4 to 7, with its header, the dates, only on page 4, and
//     some cells blank (ferritin, iron and B12 weren't tested every time);
//   - visit notes with vitals in running text, eight pages of patient
//     handouts and a consent form with no results, a medication list, an
//     allergy list and vaccinations.
//
// LabDates are the table's columns, and Labs its rows.
var LabDates = []string{"2025-03-10", "2025-09-12", "2026-03-12"}

// Lab is one row of the cumulative table; an empty value wasn't tested.
type Lab struct {
	Test, Unit, Range string
	Values            [3]string
}

// Labs are the table's rows, with section names as rows of their own.
var Labs = []Lab{
	{Test: "HEMATOLOGY"},
	{"WBC", "x10^3/uL", "4.0-11.0", [3]string{"6.2", "5.8", "6.0"}},
	{"RBC", "x10^6/uL", "3.9-5.2", [3]string{"4.5", "4.1", "4.4"}},
	{"Hemoglobin", "g/dL", "12.0-15.5", [3]string{"12.9", "11.8 L", "12.6"}},
	{"Hematocrit", "%", "35-45", [3]string{"38.9", "35.6", "37.8"}},
	{"Platelets", "x10^3/uL", "150-400", [3]string{"245", "260", "251"}},
	{"MCV", "fL", "80-100", [3]string{"88", "82", "86"}},
	{Test: "CHEMISTRY"},
	{"Glucose", "mg/dL", "70-99", [3]string{"98", "101 H", "104 H"}},
	{"BUN", "mg/dL", "7-20", [3]string{"14", "15", "13"}},
	{"Creatinine", "mg/dL", "0.6-1.1", [3]string{"0.8", "0.9", "0.9"}},
	{"Sodium", "mmol/L", "135-145", [3]string{"139", "140", "138"}},
	{"Potassium", "mmol/L", "3.5-5.1", [3]string{"4.2", "4.0", "4.4"}},
	{"Chloride", "mmol/L", "98-107", [3]string{"102", "103", "101"}},
	{"CO2", "mmol/L", "22-29", [3]string{"25", "24", "26"}},
	{"Calcium", "mg/dL", "8.6-10.3", [3]string{"9.4", "9.2", "9.5"}},
	{"Total protein", "g/dL", "6.0-8.3", [3]string{"7.1", "6.9", "7.0"}},
	{"Albumin", "g/dL", "3.5-5.0", [3]string{"4.3", "4.1", "4.2"}},
	{"ALT", "U/L", "7-35", [3]string{"22", "19", "24"}},
	{"AST", "U/L", "10-35", [3]string{"20", "18", "21"}},
	{Test: "LIPIDS"},
	{"Cholesterol, total", "mg/dL", "0-199", [3]string{"238 H", "233 H", "229 H"}},
	{"Triglycerides", "mg/dL", "0-149", [3]string{"142", "138", "131"}},
	{"HDL cholesterol", "mg/dL", ">39", [3]string{"55", "56", "58"}},
	{"LDL cholesterol", "mg/dL", "0-99", [3]string{"165 H", "158 H", "151 H"}},
	{Test: "ENDOCRINE AND VITAMINS"},
	{"Hemoglobin A1c", "%", "4.0-5.6", [3]string{"5.6", "5.7 H", "5.8 H"}},
	{"TSH", "uIU/mL", "0.4-4.0", [3]string{"1.9", "2.0", "2.2"}},
	{"Vitamin D, 25-OH", "ng/mL", "30-100", [3]string{"18 L", "24 L", "26 L"}},
	{Test: "IRON STUDIES"},
	{"Ferritin", "ng/mL", "30-400", [3]string{"", "18 L", "41"}},
	{"Iron", "ug/dL", "50-170", [3]string{"", "38 L", "72"}},
	{"Vitamin B12", "pg/mL", "200-900", [3]string{"", "410", ""}},
}

// LabValues is how many values the table holds.
func LabValues() int {
	n := 0
	for _, l := range Labs {
		for _, v := range l.Values {
			if v != "" {
				n++
			}
		}
	}
	return n
}

const pages = 24

func frame(n int) Page {
	return Page{
		{X: 50, Y: 765, Size: 11, S: "Riverside Medical Group - Health Records Release"},
		{X: 50, Y: 750, S: "Patient: EXAMPLE, JORDAN"}, {X: 260, Y: 750, S: "DOB: 06/02/1979"}, {X: 400, Y: 750, S: "MRN: 00482913"},
		{X: 50, Y: 35, Size: 8, S: "Printed 10/02/2026 09:14"}, {X: 260, Y: 35, Size: 8, S: fmt.Sprintf("Page %d of %d", n, pages)},
		{X: 400, Y: 35, Size: 8, S: "Confidential health information"},
	}
}

// writer fills pages top to bottom.
type writer struct {
	pages []Page
	y     float64
}

func (w *writer) page() {
	w.pages = append(w.pages, frame(len(w.pages)+1))
	w.y = 715
}

func (w *writer) at(x float64, s string, size float64) {
	w.pages[len(w.pages)-1] = append(w.pages[len(w.pages)-1], Text{X: x, Y: w.y, S: s, Size: size})
}

func (w *writer) line(s string) {
	w.at(50, s, 10)
	w.y -= 14
}

func (w *writer) heading(s string) {
	w.y -= 6
	w.at(50, s, 12)
	w.y -= 20
}

func (w *writer) cells(xs []float64, cs ...string) {
	for i, c := range cs {
		if c != "" {
			w.at(xs[i], c, 10)
		}
	}
	w.y -= 15
}

func (w *writer) para(s string) {
	words := strings.Fields(s)
	var l []string
	n := 0
	for _, word := range words {
		if n+len(word) > 95 {
			w.line(strings.Join(l, " "))
			l, n = nil, 0
		}
		l = append(l, word)
		n += len(word) + 1
	}
	if len(l) > 0 {
		w.line(strings.Join(l, " "))
	}
	w.y -= 8
}

var handouts = []string{
	"Your care team put this sheet together to help you understand your results and the steps you can take between visits. It is general information and does not replace advice from your own clinician, who knows your history.",
	"Keeping a steady routine helps. Try to take medicines at the same time each day, keep a written list of everything you take, and bring that list to every appointment, including vitamins and anything bought without a prescription.",
	"Moving more is one of the most useful things you can do for your heart. A brisk walk most days of the week counts. Start where you are and add a few minutes at a time; any amount is better than none.",
	"Choose foods that are mostly plants: vegetables, fruit, beans, whole grains and nuts. Limit foods high in salt, added sugar and saturated fat. Reading labels takes a little practice and gets quicker with time.",
	"Sleep matters too. Most adults do best with seven or more hours a night. A regular bedtime, a dark and quiet room, and putting screens away an hour before bed can all help you fall asleep and stay asleep.",
	"Call the clinic if you notice new symptoms, if a medicine seems to cause side effects, or if you have questions about your results. For chest pain, trouble breathing, or signs of a stroke, call emergency services right away.",
	"Iron helps your body make healthy red blood cells. If you were asked to take an iron supplement, taking it with food can ease an upset stomach, and taking it apart from calcium, antacids, tea and coffee helps it absorb.",
	"Home blood pressure readings help your care team see how you are doing between visits. Sit quietly for five minutes first, keep your arm supported at heart level, and write down each reading with the date and time.",
}

// InventedRecords is the release as a PDF.
func InventedRecords() []byte {
	w := &writer{}
	w.page() // 1
	w.heading("HEALTH RECORDS RELEASE")
	w.para("This release was prepared at the request of the patient. It contains copies of selected records from Riverside Medical Group. Each section starts on the page shown below.")
	for _, s := range []string{"Visit note, 03/10/2025 .......... 2", "Cumulative laboratory results .......... 4",
		"Progress note, 09/12/2025 .......... 8", "Patient education materials .......... 10", "Authorization and consent .......... 18",
		"Medications and allergies .......... 19", "Immunizations .......... 21", "Visit note, 03/12/2026 .......... 22"} {
		w.line(s)
	}

	w.page() // 2
	w.heading("VISIT NOTE")
	w.line("Date of service: 03/10/2025")
	w.line("Provider: Dana Whitfield, MD, Internal Medicine")
	w.heading("Reason for visit:")
	w.para("Annual physical. The patient feels well overall and reports mild tiredness in the afternoons. No chest pain, shortness of breath, or swelling. Takes lisinopril every morning and checks blood pressure at home a few times a week.")
	w.heading("Vitals:")
	vx := []float64{60, 220}
	w.cells(vx, "Blood pressure", "132/84 mmHg")
	w.cells(vx, "Heart rate", "72 bpm")
	w.cells(vx, "Weight", "171.0 lb")
	w.cells(vx, "Temperature", "98.4 F")
	w.heading("History:")
	w.para("High blood pressure diagnosed in 2021. Allergic to penicillin (hives). Family history of heart disease in her father. Does not smoke; drinks alcohol rarely. Walks two or three times a week.")
	w.page() // 3
	w.heading("Assessment and plan:")
	w.para("1. Hypertension: reasonably controlled. Continue lisinopril 10 mg daily. Keep logging home readings.")
	w.para("2. Cholesterol: fasting labs drawn today; will review the lipid panel and discuss diet and activity, and whether a statin makes sense.")
	w.para("3. Health maintenance: flu vaccine due in the fall. Tdap is up to date. Discussed sleep and exercise.")
	w.para("Follow up in six months, sooner if needed. Electronically signed by Dana Whitfield, MD.")

	// 4 to 7: the cumulative table, its header only on page 4.
	xs := []float64{50, 205, 275, 345, 420, 490}
	w.page()
	w.heading("CUMULATIVE LABORATORY RESULTS")
	w.line("Specimens collected at Riverside Medical Group laboratory. Results flagged H (high) or L (low).")
	w.y -= 6
	w.cells(xs, "Test", "03/10/2025", "09/12/2025", "03/12/2026", "Units", "Reference")
	breaks := map[string]bool{"Glucose": true, "Cholesterol, total": true, "Ferritin": true} // the first row on pages 5, 6 and 7
	for _, l := range Labs {
		if breaks[l.Test] {
			w.page()
		}
		if l.Unit == "" {
			w.y -= 4
			w.line(l.Test)
			continue
		}
		w.cells(xs, l.Test, l.Values[0], l.Values[1], l.Values[2], l.Unit, l.Range)
	}
	w.y -= 10
	w.line("End of cumulative results.")

	w.page() // 8
	w.heading("PROGRESS NOTE")
	w.line("Date of service: 09/12/2025")
	w.line("Provider: Dana Whitfield, MD")
	w.para("Seen for follow-up of tiredness, which has been worse over the summer. Labs today show a low hemoglobin of 11.8 and a low ferritin of 18, consistent with iron deficiency. No black stools or heavy bleeding reported. Blood pressure is good on lisinopril.")
	w.heading("Vitals:")
	w.cells(vx, "Blood pressure", "128/82 mmHg")
	w.cells(vx, "Heart rate", "70 bpm")
	w.cells(vx, "Weight", "168.4 lb")
	w.page() // 9
	w.heading("Plan:")
	w.para("Start ferrous sulfate 325 mg by mouth once daily with food. Recheck blood count and iron studies in about six months. Continue lisinopril 10 mg daily. Flu vaccine given next month at the pharmacy is fine.")
	w.para("Electronically signed by Dana Whitfield, MD.")

	for i := 0; i < 8; i++ { // 10 to 17: handouts, no results
		w.page()
		w.heading("PATIENT EDUCATION")
		for k := 0; k < 6; k++ {
			w.para(handouts[(i+k)%len(handouts)])
		}
	}

	w.page() // 18
	w.heading("AUTHORIZATION AND CONSENT")
	for k := 0; k < 4; k++ {
		w.para("I authorize Riverside Medical Group to release copies of my health records as described in this request. I understand that I may revoke this authorization in writing at any time, except where action has already been taken in reliance on it.")
	}
	w.line("Signed: Jordan Example                 Date: 09/28/2026")

	mx := []float64{50, 190, 250, 400, 480}
	w.page() // 19
	w.heading("MEDICATIONS")
	w.cells(mx, "Medication", "Dose", "Directions", "Started", "Status")
	w.cells(mx, "Lisinopril", "10 mg", "1 tablet by mouth daily", "01/15/2022", "Active")
	w.cells(mx, "Ferrous sulfate", "325 mg", "1 tablet daily with food", "09/12/2025", "Active")
	w.cells(mx, "Azithromycin", "250 mg", "As directed for 5 days", "02/03/2024", "Completed")
	w.page() // 20
	w.heading("ALLERGIES")
	w.cells([]float64{50, 200}, "Penicillin", "Hives")
	w.cells([]float64{50, 200}, "Latex", "Contact dermatitis")

	w.page() // 21
	w.heading("IMMUNIZATIONS")
	ix := []float64{50, 250, 350}
	w.cells(ix, "Vaccine", "Date given", "Given by")
	w.cells(ix, "Influenza, seasonal", "10/05/2025", "Pharmacy")
	w.cells(ix, "Tdap", "04/12/2021", "Riverside Medical Group")
	w.cells(ix, "COVID-19 booster", "10/05/2025", "Pharmacy")

	w.page() // 22
	w.heading("VISIT NOTE")
	w.line("Date of service: 03/12/2026")
	w.line("Provider: Dana Whitfield, MD")
	w.para("Follow-up after six months of iron. Feels more energetic. Hemoglobin is back to 12.6 and ferritin to 41. LDL remains above goal at 151; discussed starting a statin and agreed to revisit after another three months of diet changes.")
	w.heading("Vitals:")
	w.cells(vx, "Blood pressure", "126/80 mmHg")
	w.cells(vx, "Weight", "166.8 lb")

	w.page() // 23
	w.line("This page intentionally left blank.")
	w.page() // 24
	w.heading("END OF RECORDS")
	w.para("This is the end of the records included in this release. Questions about these records can be directed to the Riverside Medical Group health information office.")
	return Write(w.pages)
}
