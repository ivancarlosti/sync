// The SKU catalogue the per-user licence lookup of the report walks.
//
// The Enterprise License Manager API has no "licences of this user" read. It
// lists the assignments of a *product* (`licenseAssignments.listForProduct`),
// which takes a customer id and which a domain refuses with `403 Unauthorized
// operation for the given domain` when the caller does not manage its paid
// edition directly (a resold tenant, a domain behind a distributor). The
// per-user read of the same API (`licenseAssignments.get`) takes no customer id
// and is answered for a licence the caller can see, so a user's licences can be
// read SKU by SKU instead — GAM does exactly that in `Info_User_Licenses` /
// `getUserLicenses`.
//
// Which SKUs to ask about cannot be discovered: every listing of the catalogue is
// the same domain-wide call that was refused. The table below is therefore a
// copy of Google's public SKU catalogue — the one GAM ships as `glskus.py`,
// grouped by the products of `reportLicenseProducts` — and a licence of a SKU
// that is not in it is invisible to that fallback (report.go).
//
// The display name sits next to the ids for one reason: an answer of the per-user
// read carries `skuName` itself, but an answer that omits it still leaves the
// licenses column readable. The names are Google's own, not a translation.
package google

// reportLicenseSku is one SKU a licence can be assigned from: the product and SKU
// ids `licenseAssignments.get` interpolates into its path, and the name Google
// displays for the SKU.
type reportLicenseSku struct {
	Product string
	Sku     string
	Name    string
}

// reportLicenseSkus is the catalogue the per-user licence fallback probes, one
// batch per user: the paid editions, the storage add-ons and the add-ons Google
// sells next to them, in the order of `reportLicenseProducts`.
//
// It is data, not policy. A SKU Google adds after this list was written is not
// probed; a SKU the tenant cannot own answers "not found", which costs a part of
// a batch that was paid for anyway.
var reportLicenseSkus = []reportLicenseSku{
	{Product: "Google-Apps", Sku: "1010020020", Name: "Google Workspace Enterprise Plus (formerly G Suite Enterprise)"},
	{Product: "Google-Apps", Sku: "1010020025", Name: "Google Workspace Business Plus"},
	{Product: "Google-Apps", Sku: "1010020026", Name: "Google Workspace Enterprise Standard"},
	{Product: "Google-Apps", Sku: "1010020027", Name: "Google Workspace Business Starter"},
	{Product: "Google-Apps", Sku: "1010020028", Name: "Google Workspace Business Standard"},
	{Product: "Google-Apps", Sku: "1010020029", Name: "Workspace Enterprise Starter"},
	{Product: "Google-Apps", Sku: "1010020030", Name: "Google Workspace Frontline Starter"},
	{Product: "Google-Apps", Sku: "1010020031", Name: "Google Workspace Frontline Standard"},
	{Product: "Google-Apps", Sku: "1010020034", Name: "Google Workspace Frontline Plus"},
	{Product: "Google-Apps", Sku: "1010060003", Name: "Google Workspace Enterprise Essentials"},
	{Product: "Google-Apps", Sku: "1010060005", Name: "Google Workspace Enterprise Essentials Plus"},
	{Product: "Google-Apps", Sku: "1010070001", Name: "Google Workspace for Education Fundamentals"},
	{Product: "Google-Apps", Sku: "1010070004", Name: "Google Workspace for Education Gmail Only"},
	{Product: "Google-Apps", Sku: "Google-Apps", Name: "G Suite Legacy"},
	{Product: "Google-Apps", Sku: "Google-Apps-For-Business", Name: "G Suite Basic"},
	{Product: "Google-Apps", Sku: "Google-Apps-For-Education", Name: "Google Workspace for Education - Fundamentals"},
	{Product: "Google-Apps", Sku: "Google-Apps-For-Government", Name: "Google Workspace Government"},
	{Product: "Google-Apps", Sku: "Google-Apps-For-Postini", Name: "Google Apps Message Security"},
	{Product: "Google-Apps", Sku: "Google-Apps-Lite", Name: "G Suite Lite"},
	{Product: "Google-Apps", Sku: "Google-Apps-Unlimited", Name: "G Suite Business"},
	{Product: "Google-Drive-storage", Sku: "Google-Drive-storage-16TB", Name: "Google Drive Storage 16TB"},
	{Product: "Google-Drive-storage", Sku: "Google-Drive-storage-1TB", Name: "Google Drive Storage 1TB"},
	{Product: "Google-Drive-storage", Sku: "Google-Drive-storage-200GB", Name: "Google Drive Storage 200GB"},
	{Product: "Google-Drive-storage", Sku: "Google-Drive-storage-20GB", Name: "Google Drive Storage 20GB"},
	{Product: "Google-Drive-storage", Sku: "Google-Drive-storage-2TB", Name: "Google Drive Storage 2TB"},
	{Product: "Google-Drive-storage", Sku: "Google-Drive-storage-400GB", Name: "Google Drive Storage 400GB"},
	{Product: "Google-Drive-storage", Sku: "Google-Drive-storage-4TB", Name: "Google Drive Storage 4TB"},
	{Product: "Google-Drive-storage", Sku: "Google-Drive-storage-50GB", Name: "Google Drive Storage 50GB"},
	{Product: "Google-Drive-storage", Sku: "Google-Drive-storage-8TB", Name: "Google Drive Storage 8TB"},
	{Product: "Google-Vault", Sku: "Google-Vault", Name: "Google Vault"},
	{Product: "Google-Vault", Sku: "Google-Vault-Former-Employee", Name: "Google Vault - Former Employee"},
	{Product: "Google-Chrome-Device-Management", Sku: "Google-Chrome-Device-Management", Name: "Google Chrome Device Management"},
	{Product: "101001", Sku: "1010010001", Name: "Cloud Identity Free"},
	{Product: "101005", Sku: "1010050001", Name: "Cloud Identity Premium"},
	{Product: "101006", Sku: "1010060001", Name: "Google Workspace Essentials (formerly G Suite Essentials)"},
	{Product: "101031", Sku: "1010310002", Name: "Google Workspace for Education Plus - Legacy"},
	{Product: "101031", Sku: "1010310003", Name: "Google Workspace for Education Plus - Legacy (Student)"},
	{Product: "101031", Sku: "1010310005", Name: "Google Workspace for Education Standard"},
	{Product: "101031", Sku: "1010310006", Name: "Google Workspace for Education Standard (Staff)"},
	{Product: "101031", Sku: "1010310007", Name: "Google Workspace for Education Standard (Extra Student)"},
	{Product: "101031", Sku: "1010310008", Name: "Google Workspace for Education Plus"},
	{Product: "101031", Sku: "1010310009", Name: "Google Workspace for Education Plus (Staff)"},
	{Product: "101031", Sku: "1010310010", Name: "Google Workspace for Education Plus (Extra Student)"},
	{Product: "101033", Sku: "1010330002", Name: "Google Voice Premier"},
	{Product: "101033", Sku: "1010330003", Name: "Google Voice Starter"},
	{Product: "101033", Sku: "1010330004", Name: "Google Voice Standard"},
	{Product: "101034", Sku: "1010340001", Name: "Google Workspace Enterprise Plus - Archived User"},
	{Product: "101034", Sku: "1010340002", Name: "Google Workspace Business - Archived User"},
	{Product: "101034", Sku: "1010340003", Name: "Google Workspace Business Plus - Archived User"},
	{Product: "101034", Sku: "1010340004", Name: "Google Workspace Enterprise Standard - Archived User"},
	{Product: "101034", Sku: "1010340005", Name: "Google Workspace Business Starter - Archived User"},
	{Product: "101034", Sku: "1010340006", Name: "Google Workspace Business Standard - Archived User"},
	{Product: "101034", Sku: "1010340007", Name: "Google Workspace for Education Fundamentals - Archived User"},
	{Product: "101035", Sku: "1010350001", Name: "Cloud Search"},
	{Product: "101036", Sku: "1010360001", Name: "Google Meet Global Dialing"},
	{Product: "101037", Sku: "1010370001", Name: "Google Workspace for Education: Teaching and Learning Upgrade"},
	{Product: "101038", Sku: "1010380001", Name: "AppSheet Core"},
	{Product: "101038", Sku: "1010380002", Name: "AppSheet Enterprise Standard"},
	{Product: "101038", Sku: "1010380003", Name: "AppSheet Enterprise Plus"},
	{Product: "101039", Sku: "1010390001", Name: "Assured Controls"},
	{Product: "101039", Sku: "1010390002", Name: "Assured Controls Plus"},
	{Product: "101040", Sku: "1010400001", Name: "Chrome Enterprise Premium"},
	{Product: "101043", Sku: "1010430001", Name: "Google Workspace Additional Storage 10TB"},
	{Product: "101043", Sku: "1010430002", Name: "Google Workspace Additional Storage 100GB"},
	{Product: "101043", Sku: "1010430003", Name: "Google Workspace Additional Storage 1TB"},
	{Product: "101047", Sku: "1010470001", Name: "Gemini Enterprise - Legacy"},
	{Product: "101047", Sku: "1010470002", Name: "Google Workspace Labs"},
	{Product: "101047", Sku: "1010470003", Name: "Gemini Business"},
	{Product: "101047", Sku: "1010470004", Name: "Google AI Pro for Education"},
	{Product: "101047", Sku: "1010470005", Name: "Gemini Education Premium"},
	{Product: "101047", Sku: "1010470006", Name: "AI Security"},
	{Product: "101047", Sku: "1010470007", Name: "AI Meetings and Messaging"},
	{Product: "101047", Sku: "1010470008", Name: "Google AI Ultra for Business"},
	{Product: "101047", Sku: "1010470009", Name: "AI Expanded Access"},
	{Product: "101049", Sku: "1010490001", Name: "Endpoint Education Upgrade"},
	{Product: "101050", Sku: "1010500001", Name: "Colab Pro"},
	{Product: "101050", Sku: "1010500002", Name: "Colab Pro+"},
}

// licenseSkuName answers the display name the catalogue carries for a SKU, or an
// empty string when the SKU is not in it: the fallback of an answer that carries
// no `skuName`.
func licenseSkuName(sku string) string {
	for _, entry := range reportLicenseSkus {
		if entry.Sku == sku {
			return entry.Name
		}
	}
	return ""
}
