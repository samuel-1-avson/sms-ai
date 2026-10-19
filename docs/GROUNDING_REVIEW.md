# Grounding store review

Source check of every snippet in `data/ghana_grounding_seed.yaml` (plan §7.5).
These answers go to users by SMS without the model, so a wrong code or
procedure is a real harm. Use this document to decide, entry by entry, whether
to flip `reviewed: true`.

- **Date accessed:** 2026-10-17 for every source below, unless a row says
  otherwise. Publication dates are given where the source shows one.
- **Method:** primary sources first (NHIA, NCA, MTN, Telecel, AT, ECG, GRA,
  MFA passport portal, WAEC/GES, police, fire and ambulance services). News
  reports are used only where they quote the agency directly or where no
  primary page exists.
- **Verdicts:** *confirmed* (a primary source supports the answer as written),
  *contradicted* (a primary source says otherwise; the seed was changed),
  *could not verify* (no primary source found; the seed was left alone).
- **Seed changes:** only `ghana-card-sim-link` was changed (`*400#` to
  `*404#`), because the NCA and MTN both contradict `*400#`. Every entry's
  `source` field now cites its sources. Every entry is still `reviewed: false`.
- **Proposed answers** are plain GSM-7, at most 160 septets, no curly quotes.
  Only the SIM-link answer was applied. The others are optional improvements
  for the reviewer to accept or ignore.

## Summary

| Slug | Claim (current answer, short) | Sources | Accessed | Verdict | Proposed answer |
|---|---|---|---|---|---|
| `nhis-check-renew` | Dial \*929# on any network, or visit an NHIS district office with Ghana Card or NHIS card | [NHIA membership](https://nhis.gov.gh/membership); [Graphic, 2019-12-16](https://www.graphic.com.gh/news/health/ghana-news-five-million-renew-nhis-on-mobile-phone.html) | 2026-10-17 | Confirmed (minor caveats) | Optional: `To check or renew NHIS, dial *929# or use the MyNHIS app, or visit any NHIS district office. Pregnant women and indigents should renew at the office.` (149) |
| `ghana-card-sim-link` | Dial **\*400#** to link Ghana Card to SIM, or visit service centre | [NCA registration process](https://nca.org.gh/registration-process/); [NCA app guide v9 (PDF)](https://nca.org.gh/wp-content/uploads/2022/08/GH-SIM-REG-APP-STEPS-v9.pdf); [MTN help centre](https://help-center.mtn.com.gh/hc/en-us/articles/7526277616658-I-am-currently-not-in-Ghana-but-I-have-my-Ghana-Card-How-do-I-register-my-SIM); [NCA numbering plan (PDF)](https://www.nca.org.gh/wp-content/uploads/2021/11/NUMBERING-PLAN-FOR-GHANA.pdf); [NCA 2026 SIM FAQ (PDF)](https://nca.org.gh/wp-content/uploads/2026/03/SIM-Reg-FAQs-2026.pdf) | 2026-10-17 | **Contradicted** (code is \*404#) | **Applied:** `To link your Ghana Card to your SIM, dial *404# and follow the prompts, then finish registration at your network's service centre with your Ghana Card.` (151) |
| `ecg-prepaid` | Buy via \*226#, MoMo or vending point; report faults at an ECG office | [ECG notice, 2025-12-29](https://www.ecg.com.gh/index.php/en/media-centre/public-notices/re-technical-challenge-with-mms-compliant-meters) (text via [allAfrica](https://allafrica.com/stories/202512290441.html)); [ECG home page](https://www.ecg.com.gh/) | 2026-10-17 | Confirmed | Optional: `Buy ECG prepaid credit by dialing *226#, the ECG Power App, MoMo or an ECG vending point. Report faults on 0302611611 or at your nearest ECG office.` (148) |
| `momo-pin-safety` | Never share MoMo PIN or OTP; your network will never ask for your PIN | [MTN help centre: keep wallet safe](https://help-center.mtn.com.gh/hc/en-us/articles/36643577740690-How-Can-I-Keep-My-MoMo-Wallet-Safe); [Graphic, 2025-04-15 (MTN MoMo statement)](https://www.graphic.com.gh/business/business-news/mtn-momo-denies-system-breach-in-viral-ghc11-000-fraud-claim-cites-customer-negligence.html); [Citi, 2025-05-02 (MoMo risk officer)](https://citinewsroom.com/2025/05/momo-fraud-our-staff-cant-access-your-pin-godwin-tamakloe/); [GNA, 2026-09-22](https://gna.org.gh/2026/09/ghana-chamber-of-telecommunications-urges-customers-to-guard-momo-pins-against-rising-fraud-cases/) | 2026-10-17 | Confirmed (MTN); Telecel and AT not found | None |
| `momo-wrong-transfer` | Wrong transfer or scam: call your network on 100 at once and ask for a reversal | [MTN: self-initiated reversal](https://help-center.mtn.com.gh/hc/en-us/articles/28353061724946-Self-Initiated-MoMo-Reversal); [MTN: report MoMo fraud](https://help-center.mtn.com.gh/hc/en-us/articles/4405588071314-How-do-I-report-Mobile-Money-MoMo-fraud); [MTN: complaints, call 100](https://help-center.mtn.com.gh/hc/en-us/articles/5336768474386-Where-do-I-report-complaint-or-make-enquiries); [Telecel contact](https://telecel.com.gh/contact-us/); [AT customer care](https://www.at.com.gh/personal/customer-care) | 2026-10-17 | Confirmed, but incomplete | Optional: `Sent MoMo to the wrong number or were scammed? Call your network on 100 at once. On MTN you can also dial *170# > My Wallet > Reversal. Never share your PIN.` (157) |
| `telco-customer-care` | MTN, Telecel and AT customer care: call 100 | [MTN: complaints, call 100](https://help-center.mtn.com.gh/hc/en-us/articles/5336768474386-Where-do-I-report-complaint-or-make-enquiries); [Telecel contact](https://telecel.com.gh/contact-us/); [AT customer care](https://www.at.com.gh/personal/customer-care); [NCA numbering plan (PDF)](https://www.nca.org.gh/wp-content/uploads/2021/11/NUMBERING-PLAN-FOR-GHANA.pdf) | 2026-10-17 | Confirmed | None |
| `emergency-numbers` | 112 general, 191 police, 192 fire, 193 ambulance | [NCA numbering plan (PDF)](https://www.nca.org.gh/wp-content/uploads/2021/11/NUMBERING-PLAN-FOR-GHANA.pdf); [GNA, 2025-12-31 (Police)](https://gna.org.gh/2025/12/ghana-police-deploy-personnel-for-31st-watch-night-services/); [Rainbow Radio, 2026-05-26 (GNFS)](https://rainbowradioonline.com/?p=167764); [Citi, July 2026 (NAS)](https://www.citinewsroom.com/2026/07/equip-ambulances-beyond-stretchers-and-sirens-ambulance-service-appeals-to-govt/) | 2026-10-17 | Confirmed (193 only via NCA) | Optional: `Ghana emergency numbers: 112 for any emergency, 191 police, 192 fire service, 193 ambulance. Police also on 18555.` (114) |
| `passport-application` | Apply at passport.mfa.gov.gh, pay, attend PAC appointment with Ghana Card | [MFA passport portal](https://passport.mfa.gov.gh/); [MFA passport FAQs](https://passport.mfa.gov.gh/faqs) | 2026-10-17 | Confirmed | None |
| `gra-tin` | For individuals, Ghana Card PIN is the GRA TIN | [GRA TIN FAQ](https://gra.gov.gh/tin/tin-faq/); [GRA commencement notice](https://gra.gov.gh/commencement-of-the-use-of-ghana-card-personal-identification-number-as-taxpayer-identification-number/) | 2026-10-17 | Confirmed | None |
| `wassce-grading` | A1 75-100 ... F9 0-39; A1-C6 credits | [Wikipedia: WASSCE](https://en.wikipedia.org/wiki/West_African_Senior_School_Certificate_Examination) (secondary); [WAEC Ghana](https://waecgh.org/) (no grading page found) | 2026-10-17 | **Could not verify** (score bands) | Optional: `WASSCE grades run from A1 (excellent) to F9 (fail). A1 to C6 are credit passes, D7 and E8 are passes and F9 is a fail.` (118) |
| `bece-aggregate` | 4 core subjects + best 2 others; grades 1-9; best aggregate 6 | [GES statement via GBC, 2025-09-02](https://www.gbcghanaonline.com/news/education/ges-aggregates/2025/); [MyJoyOnline, Sept 2026 (CSSPS)](https://www.myjoyonline.com/about-17-of-2026-bece-candidates-had-aggregates-6-18-cssps/) | 2026-10-17 | Confirmed | None |
| `ohms-law` | V = I x R | [OpenStax College Physics 2e, 20.2](https://openstax.org/books/college-physics-2e/pages/20-2-ohms-law-resistance-and-simple-circuits) | 2026-10-17 | Confirmed | None |
| `newtons-first-law` | At rest or constant speed in a straight line unless an unbalanced force acts | [NASA Glenn: Newton's laws](https://www1.grc.nasa.gov/beginners-guide-to-aeronautics/newtons-laws-of-motion/) | 2026-10-17 | Confirmed | None |

Numbers in brackets after a proposed answer are its GSM-7 septet count.

## Notes per snippet

### nhis-check-renew: confirmed (minor caveats)

- NHIA's membership page says membership can be renewed "at any NHIS District
  office or by using the USSD (\*929#) or "MyNHIS" App". It also says pregnant
  women and indigents are exempt from the digital platforms because those
  channels require payment, so they should renew at the office.
- NHIA's page does not describe checking status. The NHIA deputy CEO, quoted
  in the Graphic (2019-12-16), said the \*929# service lets members "check
  policy validity" and renew from a mobile money wallet.
- Not verified: "on any network" (neither source names networks) and "with
  your Ghana Card or NHIS card" (NHIA says members "present their membership
  details"; Ghana Card details are required at enrolment). Neither is
  contradicted.
- The optional answer drops the two unverified phrases, adds the MyNHIS app,
  and sends exempt groups to the office.

### ghana-card-sim-link: contradicted, seed changed

- **The code is \*404#, not \*400#.** The NCA's registration-process page says
  existing subscribers start "by submitting Ghana Card details ... via USSD
  Short Code 404". The NCA's GH SIM Self Reg app guide (v9) says "Make sure you
  have completed STAGE 1 (\*404#)". MTN's help centre says "Just dial \*404# to
  link your Ghana Card with your number".
- **Where \*400# came from:** the NCA numbering plan lists harmonised code 400
  as "VERIFICATION OF SIM REGISTRATION", which is the old (2011-era) check that
  a SIM is registered. It does not link a Ghana Card. One secondary guide
  (yen.com.gh) says \*400# shows the unique code after stage 1. No primary
  source supports \*400# for linking.
- **Two stages:** after \*404#, the subscriber gets a unique code and must
  finish with biometrics, at the network's service centre or agent, or (at
  GHS 5) in the GH SIM Self Reg app. NCA Consumer Watch ed. 8 says
  registration at the operator's shop or agent is free. The applied answer
  mentions the service centre, which works for everyone.
- **Re-review trigger, important:** the main 2021-2023 exercise closed on
  31 May 2023, though the Telecoms Chamber said in June 2023 that deactivated
  SIMs could still be registered. A **new biometric re-registration** is
  coming. The NCA-hosted Ministry FAQ (March 2026) describes real-time
  biometric checks against the NIA database, a "Primary Number" for OTPs and
  CEIR device checks. On 7 September 2026 the Communications Minister said it
  would start "before the end of 2026" and that Parliament had passed the
  Legislative Instrument
  ([MyJoyOnline](https://www.myjoyonline.com/sim-registration-biometric-verification-to-begin-before-end-of-2026-sam-george/)).
  No start date, short code or app had been announced as of 2026-10-17. When it
  launches, this answer will probably need rewriting and \*404# may stop
  applying. Re-check NCA notices before flipping `reviewed`, and again at
  launch.
- Whether \*404# still accepts stage-1 requests today could not be tested from
  here (it needs a Ghana SIM). The NCA page still lists it.

### ecg-prepaid: confirmed

- ECG public notice of 2025-12-29 (text via allAfrica; the ECG page renders it
  as an image): customers "can now successfully purchase electricity credits
  for their MMS- Compliant Meters via the ECG Power App/Short code \*226# and
  from third-party vendors".
- ECG's home page: report faults "to the ECG Call Center on 0302-611611, the
  nearest ECG office, or ... social media". "Report faults at your nearest
  ECG office" is correct. The optional answer adds the call centre number and
  the Power App.
- Reviewer note: the December 2025 notice is about smart (MMS-compliant)
  meters. Customers with older card-based prepaid meters may still need a
  vending point to load credit. ECG did not say this; check it with ECG if it
  matters.

### momo-pin-safety: confirmed (MTN); Telecel and AT not found

- MTN help centre (updated 2026-07-02): "Never disclose your MoMo PIN to
  anyone." MTN MoMo's April 2025 statement: "Never share your PIN or OTP with
  anyone - not even someone claiming to be from MTN". MoMo's Chief Risk and
  Compliance Officer (May 2025): "Our staff do not have access to your pins."
- The Ghana Chamber of Telecommunications (September 2026) warned against
  sharing a MoMo PIN "through any channel".
- "Your network will never ask for your PIN" is not word for word in an MTN
  source, but it follows from the sources above. I found no Telecel Cash or AT
  Money page saying this; the advice is industry-standard. No change proposed.

### momo-wrong-transfer: confirmed, but incomplete

- 100 is the helpline on all three networks (see `telco-customer-care`), so
  "call your network on 100" is correct.
- MTN documents a faster self-service route: dial \*170#, option 6 "My
  Wallet", option 8 "Reversal", pick the transaction and confirm. The
  recipient is asked to approve. If they do not, email customercare.gh@mtn.com
  and MTN "will hold the money in their wallet and begin the normal reversal
  process" (article dated 2025-07-27). Fraud can be reported via \*170# > 6 >
  4 or mmfraudteam.GH@mtn.com.
- MTN's MoMo safety page (2026) calls 0244300000 "MTN's official customer
  service number". 100 still works from an MTN line, per MTN's complaints
  article.
- I found no Telecel Cash or AT Money reversal procedure on their sites.
- The Chamber of Telecommunications and police (September 2026) advise
  reporting fraud to the provider **and the police**.
- Not contradicted, so the seed is unchanged. The optional answer adds the MTN
  self-service route.

### telco-customer-care: confirmed

- MTN: "Call 100 (MTN Call Centre), visit any MTN Branch Office or any of the
  MTN Social Media and digital channel." Telecel contact page: "Helpline 100"
  (also 050 555 5111, WhatsApp 050 100 0300). AT customer-care FAQ: "Visit any
  AT shop or call 100 for assistance". The NCA numbering plan lists "CALL
  CENTER 100" as a harmonised code. NCA Consumer Watch ed. 8: "dialing 100
  across all networks".
- AT is the current brand name for the former AirtelTigo; the triggers keep
  "airteltigo", which is right for matching.

### emergency-numbers: confirmed (193 via NCA only)

- NCA numbering plan, emergency services table: Ghana Police Service 191,
  Fire Service 192, Ambulance 193, Emergency SOS 112, all "Operational".
- Police (GNA, 2025-12-31) told the public to use "18555, 191, 112, or
  0302-773906". GNFS PR officer (2026-05-26): "The emergency lines are 192,
  112, and 0302772446." The National Ambulance Service CEO (July 2026) told the
  public to dial 112 and did not mention 193.
- 112 was announced in 2019-2020 as the single number for all emergency
  services. 191 and 192 are still promoted by their agencies. 193 is listed as
  operational only by the NCA. The optional answer marks 112 as working for
  any emergency and adds the police 18555 line.

### passport-application: confirmed

- passport.mfa.gov.gh (title "Ghana Passport Services | Ministry of Foreign
  Affairs") lists four steps: start the application online, enter details, pay
  online, then "Visit your selected Passport Application Centre (PAC) for
  biometrics and receive your passport via doorstep delivery".
- FAQ: "National ID (Ghana Card)" is "Mandatory" for both first-time and
  renewal applications. Applicants also need a printed and signed form and a
  birth certificate (new) or current passport (renewal).
- Standard fee shown: GHS 350, down from GHS 500. Not in the answer, so it does
  not need re-checking when the fee changes.

### gra-tin: confirmed

- GRA TIN FAQ: "For individuals, the PIN on your Ghana Card is now your TIN."
  It took effect on 1 April 2021. Companies and organisations still use
  GRA-generated TINs, and the answer correctly limits itself to individuals.

### wassce-grading: could not verify (score bands)

- I found no WAEC Ghana page publishing percentage bands. waecgh.org has no
  grading page, and its August 2026 BECE press statement is about marking
  fairness, not grade bands.
- The bands in the seed (A1 75-100, B2 70-74 ... F9 0-39) match Wikipedia and
  most guides. Some Ghanaian sites instead give A1 80-100 and B2 70-79. With no
  primary source either way, the seed is unchanged.
- The grade meanings (A1 excellent to F9 fail; A1-C6 credit; D7-E8 pass; F9
  fail) are consistent everywhere and match the snippet's `context` field.
- Recommendation: either get the bands confirmed by WAEC Ghana in writing, or
  use the optional answer without percentages, which states only the
  consistent part.

### bece-aggregate: confirmed

- GES (Head of PR Daniel Fenyi, 2025-09-02): the aggregate adds the grades in
  the four core subjects (English, Mathematics, Integrated Science, Social
  Studies) to the best two grades from the other subjects.
- The 1-9 scale (1 best) and best aggregate of 6 are not spelled out in the GES
  statement, but CSSPS's 2026 placement figures report "aggregates 6-18",
  which fits. 2026 reports say grade 9 is now labelled "Lowest" rather than
  "Fail", which does not affect this answer.
- The answer says "best 2 electives"; GES says "best two from the other
  subjects". The meaning is the same, so no change.

### ohms-law: confirmed

- OpenStax College Physics 2e §20.2: I = V/R, so V = IR, with
  "1 ohm = 1 volt per ampere". Ohm's law strictly applies to ohmic conductors,
  which is beyond SMS scope.

### newtons-first-law: confirmed

- NASA Glenn: "An object at rest remains at rest, and an object in motion
  remains in motion at constant speed and in a straight line unless acted on
  by an unbalanced force." The answer matches.

## Reviewer checklist

1. Read the note for each slug. Flip `reviewed: true` only for entries you
   accept as written, or after applying a proposed answer you accept.
2. `ghana-card-sim-link`: re-check NCA notices for the new biometric exercise
   first. Set a reminder to revisit when it launches (expected before end-2026).
3. `wassce-grading`: get WAEC confirmation of the bands, or switch to the
   answer without percentages.
4. Phone numbers and USSD menus change. Re-run this review at least every six
   months and whenever an MNO, NCA or agency announces a change.
