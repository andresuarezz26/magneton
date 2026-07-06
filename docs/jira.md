# Jira setup (optional)

Magneton can pull a ticket's title and description straight from Jira, so you can run a ticket by key (`magneton run PROJ-123`) instead of writing a markdown file.

1. **Create an API token** at [id.atlassian.com/manage-profile/security/api-tokens](https://id.atlassian.com/manage-profile/security/api-tokens). Atlassian shows it only once.

2. **Enter it during `magneton init`:**

   | Prompt | Example |
   | --- | --- |
   | Jira base URL | `https://your-org.atlassian.net` |
   | Jira email | `you@your-org.com` |
   | Jira API token | `ATATT3xFfGF0...` |

   The token is stored in your OS keychain, not the config file. For headless or CI use, set `MAGNETON_JIRA_TOKEN` instead.

3. **Verify** with `magneton doctor` - a passing Jira check means you can run tickets by key.
