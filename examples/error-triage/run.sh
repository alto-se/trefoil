# Uses the AWS credentials the server already passes to agents (Bedrock):
# AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN. They need IAM
# permission to use the AWS MCP Server (aws-mcp:InvokeMcp,
# aws-mcp:CallReadOnlyTool, ...) and read access to the logs and services
# being debugged (e.g. CloudWatchLogsReadOnlyAccess, AWSCloudTrail_ReadOnlyAccess,
# CloudWatchReadOnlyAccess, or ReadOnlyAccess).
trefoil config sync config.yml
trefoil agents build --agent=error-triage
trefoil agents run --agent=error-triage --input=file://input.json
