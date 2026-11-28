import {boxesForPage,clampPage} from "../pdf/bbox-overlay";
const view={width:200,height:100};
it("converts normalized corner boxes, clamps and preserves page orientation",()=>{expect(clampPage(0,3)).toBe(1);expect(clampPage(8,3)).toBe(3);expect(boxesForPage([{page:1,rect:[.1,.2,.5,.8]}],1,view)).toEqual({boxes:[{left:20,top:20,width:80,height:60}],approximate:false});});
it("marks missing, full-page, malformed and out-of-range boxes approximate",()=>{expect(boxesForPage([{page:1,rect:[0,0,1,1]},{page:1,rect:[-.1,0,.2,.2]},{page:2,rect:[0,0,.2,.2]}],1,view)).toMatchObject({approximate:true,boxes:[{left:0,top:0,width:40,height:20}]});expect(boxesForPage(undefined,1,view)).toEqual({boxes:[],approximate:true});});
it("maps quarter rotations in viewport coordinates",()=>{expect(boxesForPage([{page:1,rect:[0,.2,.5,.8]}],1,{width:200,height:100,rotation:90}).boxes[0]).toEqual({left:40,top:0,width:120,height:50});});
