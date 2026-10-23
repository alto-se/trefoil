trefoil config sync config.yml
trefoil agents build --agent=release-notes
trefoil agents run --agent=release-notes --input=file://input.json
