#!/bin/bash

commit_message_check (){
    # gets the git commit message from file
    gitmessage=$(cat "$1")

    ####################### TEST STRINGS ###################################
    #gitmessage="feat(test): testtest" # success
    #gitmessage="feat(test): test test" # success
    #gitmessage="feat(core): test test" # success
    #gitmessage="feat(core): testtest" # fail
    #gitmessage="fail(core): testtest" # fail
    #gitmessage="feat(fail): testtest" # fail
    #gitmessage="featcore): testtest" # fail
    #gitmessage="feat(core: testtest" # fail
    #gitmessage="feat(core) test test" # fall
    #gitmessage="feat(core: testtest" # fail
    #gitmessage="feat(core): " # fail
    #gitmessage="feat(core):" # fail
    #gitmessage="feat(core):   " # fail
    #########################################################################

    # Checks gitmessage for string feat, fix, docs and breaking, for which component or core
    messagecheck=$(echo "$gitmessage" | grep -E "^(fix|feat|perf|refactor|test|docs|break)\((core)\)!?: .+")
    if [ -z "$messagecheck" ]
    then
        echo "Your commit message must include the type followed with the component that changed"
        echo "  <type>(<component>)!?: <some text>"
        echo "  where <type> is the type of change"
        echo "    fix: A bug fix"
        echo "    feat: A new feature"
        echo "    perf: A code change that improves performance"
        echo "    refactor: A code change that neither fixes a bug nor adds a feature"
        echo "    test: Adding missing tests or correcting existing tests"
        echo "    docs: Documentation only changes"
        echo "    break: breaking changes so release a major update"
        echo "  where <component> is the componenet that changed: (core)"
        echo ""
        echo "Please review your last commit message:"
        echo "\"$gitmessage\""
        exit 1
    fi
}

# Calling the function
commit_message_check $1
