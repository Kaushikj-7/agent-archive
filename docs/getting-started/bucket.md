# Create a bucket

agent-archive stores sessions in a private bucket you own. Create it once;
every machine you set up can share it. You need a Cloudflare or AWS account for
the bucket; there is no agent-archive account or hosted service. Cloudflare
R2 is the quickest to set up. `agent-archive setup` can create an S3 bucket
for you and an R2 one (both below), and links here when you
choose "Setup instructions" at its storage question.

## Cloudflare R2 (recommended)

Choose the route that matches what you need:

- **A new bucket:** [let setup create it](#let-setup-create-it).
  You provide one temporary Cloudflare API token; setup creates the bucket
  and its archive key.
- **An existing bucket:** start at step 2 of [the manual instructions](#create-it-by-hand),
  then choose existing R2 storage in `agent-archive setup`.

Cloudflare has two different token forms: **R2 → Manage API Tokens** creates
R2 credentials, while **Manage account → Account API tokens** lets you choose
custom account permissions. Automatic bucket creation below needs the latter.
Which one do I have? If the form gave you an **Access Key ID** and a **Secret
Access Key**, that was the R2 form, for the manual route; if it gave you a
single token value after you picked custom permission rows, that was the
account form, the one automatic creation needs.

### Let setup create it

`agent-archive setup` can create the bucket for you. At
the storage question, choose **Cloudflare R2**, then **Continue**. You
make one Cloudflare API token by hand, once; setup does the rest. R2 must
already be enabled on your Cloudflare account (Cloudflare may ask for a
payment method; see [current R2
pricing](https://developers.cloudflare.com/r2/pricing/)).

1. Open [Account API tokens in the Cloudflare
   dashboard](https://dash.cloudflare.com/?to=%2F%3Aaccount%2Fapi-tokens), sign
   in and select your account. Use **Manage account → Account API tokens**,
   rather than **R2 → Manage API Tokens**. Select **Create Token** and use
   the custom token form. Add these two permission rows:

   - **Account → Workers R2 Storage → Edit** (API name:
     `Workers R2 Storage Write`; creates the bucket).
   - **Account → Account API Tokens → Edit** (API name:
     `Account API Tokens Write`; creates and revokes the bucket's own key).

   Scope the token to your account only, review the summary, create it, and
   copy the **API token value** for setup. Cloudflare's dashboard calls write
   access **Edit**; API documentation uses **Write**. See [Cloudflare's initial
   token instructions](https://developers.cloudflare.com/fundamentals/api/how-to/create-via-api/#generating-the-initial-token).

   If you see **Create Account API token** / **Create User API token** and
   permission choices such as **Admin Read & Write** / **Object Read & Write**,
   you are in the R2-specific form. That form cannot grant both permissions
   needed by automatic setup; return to **Manage account → Account API tokens**.
   Account token creation requires API Token Provisioning capabilities or
   Super Administrator status, and members can grant only permissions they
   hold themselves ([Cloudflare's account token requirements](https://developers.cloudflare.com/fundamentals/api/get-started/account-owned-tokens/)).
   If the custom permissions aren't available, ask an account administrator
   or use the manual route below.
2. Run `agent-archive setup`, choose
   **Cloudflare R2**, then **Continue**, and paste the token when
   asked (it is hidden). If your shell sets `CLOUDFLARE_API_TOKEN` and
   `CLOUDFLARE_ACCOUNT_ID`, as Cloudflare's own tools expect, setup uses them
   and doesn't ask.
3. Setup first looks up the archive-key permission and checks
   that it can be granted. If this check fails, no bucket or key is created.
   You can paste a different token, retry after updating its permissions, or
   use an existing bucket, or go back to choose other storage. Once the lookup
   succeeds, setup shows a suggested bucket name (`agent-archive-` and eight
   random characters) and automatic location. Press Enter to create it, or
   choose **Customize** to edit the name and location and return to the summary.
   This does not guarantee that Cloudflare will accept the later token-creation
   request. After your confirmation, setup creates
   a new bucket (Cloudflare buckets have no public access by default), creates
   a second token that can read, write, and list objects in **that one bucket
   only**, checks that it works, and keeps that second key in the macOS
   Keychain like any R2 key.

If setup finds the new bucket publicly readable (its `r2.dev` URL is on, or a
custom domain serves it), it stops and asks whether to check again, choose
another storage option (the default, which revokes the new key), or continue
anyway.

The token you pasted is used during setup and then dropped: it is never
saved, in the Keychain or anywhere else, and the collector never sees it. You
can delete it in the dashboard afterwards. If setup stops partway, it names
the token it was creating, so you can revoke a leftover one; it also says
when it leaves an empty bucket behind, and if it can't store the new key it
revokes that token itself. What setup does with the token, and what it reports
about public access, is in
[privacy](../security/privacy.md#guided-r2-bucket-creation).

Setup does not set any lifecycle rule on the bucket. To have Cloudflare delete
old objects whatever happens to your Macs, add one yourself: see [a lifecycle
rule as a backstop](uninstall.md#a-lifecycle-rule-as-a-backstop).

Setup creates only a bucket you have not used: it never changes an existing
one, and it never reuses an existing name. To use a bucket you already have,
or if you prefer to do it by hand, follow the manual steps below.

### Create it by hand

1. In the [Cloudflare dashboard](https://dash.cloudflare.com), open
   **Storage & databases → R2 → Overview** ([current Cloudflare
   steps](https://developers.cloudflare.com/r2/get-started/)), enable an R2
   subscription if prompted, and create a bucket, for example
   `agent-archive`. Cloudflare may require a checkout flow. Check [current
   R2 pricing and included
   usage](https://developers.cloudflare.com/r2/pricing/) before enabling it;
   charges depend on storage class and usage. Leave public access off.
2. Under **Account Details** on the R2 overview page, choose **Manage** next
   to **API Tokens**. Select **Create Account API token** for a durable key
   tied to the account, or **Create User API token** for personal access
   (it becomes inactive if your user leaves the account). Give it
   **Object Read & Write** permission, choose **Apply to specific buckets only**,
   and select your archive bucket. Do not leave **Object Read only** or
   **Apply to all buckets** selected. Select **Create Account API token**
   (or **Create User API token**), then copy the **Access Key ID** and **Secret Access Key** (the secret
   is shown once). See Cloudflare's current [R2 token instructions](https://developers.cloudflare.com/r2/api/tokens/)
   if the dashboard wording changes.
3. Copy your **Account ID** from the R2 overview page, or the bucket's S3 API
   URL from its **Settings** (`https://<account-id>.r2.cloudflarestorage.com/<bucket>`).

Then run `agent-archive setup`, choose **Cloudflare R2**, then **Use an existing bucket**, and paste the Account ID (then
the bucket name) or the bucket's URL (which names both), and the two keys.
The secret is kept in the macOS Keychain, or on Linux, which has none, in a
private credentials file that is not encrypted ([where credentials are
kept](../security/privacy.md#where-credentials-are-kept)); on Linux an S3
profile (below) avoids storing a secret of agent-archive's own.

This route does not need **Workers R2 Storage Write** or **Account API Tokens
Write**. Those are only needed when setup creates the bucket and its key for you.
If you already have an Object Read & Write token for the archive bucket, you
can use its S3 keys. Cloudflare cannot show a lost Secret Access Key again;
create a new token if you no longer have it.

## Amazon S3

### Let setup create it

If you have an AWS profile that may create buckets, `agent-archive setup`
can do the console steps: choose **Amazon S3**, then **Continue**, pick the profile, and review the
suggested name and profile region. Choose **Customize** to change the name,
region or creation profile; press Enter on the summary to create. If your
profile has no region, setup asks for one before the summary. Setup creates
the bucket, turns on all four **Block Public Access** settings, checks them,
and prints the [least-privilege
policy](../security/bucket-permissions.md#amazon-s3) for the new bucket. The
profile needs `s3:CreateBucket` and `s3:PutBucketPublicAccessBlock` (see
[creating a bucket](../security/bucket-permissions.md#creating-a-bucket-setup-time-only));
without them setup says so and lets you pick an existing bucket. It creates
buckets in the standard AWS regions only (not China or GovCloud); pick an
existing bucket for those. Setup never creates IAM users or keys. After the
bucket is made it asks which profile archiving should use, defaulting to the
one that created it, which can do far more than archiving needs. To use a
narrower one, create a separate IAM user or role with the printed policy, save
it as its own profile, and choose it at that question (or run
`agent-archive setup` again to switch to it).

### By hand

1. Create a bucket in the S3 console. Keep **Block all public access** on
   (the default).
2. Create an IAM user (or role) with the
   [least-privilege policy](../security/bucket-permissions.md#amazon-s3) for
   that bucket, and an access key for it.
3. Save the key as an AWS profile: `aws configure --profile agent-archive`.

Then run `agent-archive setup`, choose **Amazon S3**, then **Use an existing
bucket**, and pick that profile.

## One key per machine

Creating a separate token or access key for each machine lets you revoke one
without touching the others. See [multiple machines](../guides/multiple-machines.md).
