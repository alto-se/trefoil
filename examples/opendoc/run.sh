trefoil config sync config.yml
trefoil agents build --agent=opendoc
trefoil agents run --agent=opendoc --input=file://input.json
trefoil sessions artifacts --agent=opendoc SESSION_ID -o out   # SESSION_ID from the run
