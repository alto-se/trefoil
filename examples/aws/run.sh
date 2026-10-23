# Uses the AWS credentials the server already passes to agents (Bedrock):
# AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN. They also need
# IAM permission to use the AWS MCP Server (aws-mcp:InvokeMcp, aws-mcp:CallReadOnlyTool, ...).
trefoil config sync config.yml
trefoil agents build --agent=aws
trefoil agents run --agent=aws --input=file://input.json
